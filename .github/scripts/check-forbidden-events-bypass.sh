#!/usr/bin/env bash
# AL-INV-10 / AL-EVT-1 CI gate (LLD §2.4, §3.3.3, §7.6): iam-audit-log is a
# pure sink and publishes NO bus events — no SNS publisher, no outbox, ever.
#
# Stricter than iam-authz-enrichment (which still calls LoadSNS/LoadOutbox
# for parity): here even the publisher-side *config* helpers are forbidden,
# so there is no half-wired publisher to accidentally finish. Also carries
# iam-audit-log's platform-events bypass checks (no raw SQS transport
# calls, no hand-built envelopes).
#
# Test files are exempt: integration/e2e tests legitimately act as the
# external producers (SNS publish / SQS send) this service consumes from.
set -euo pipefail

# Matches in // comments are ignored (docs may name what is forbidden).
scan() { grep -REn "$1" --include='*.go' cmd/ internal/ pkg/ 2>/dev/null | grep -v '_test\.go' | grep -vE '^[^:]+:[0-9]+:[[:space:]]*//' || true; }
fail() { echo "::error::$1"; echo "$2"; exit 1; }

# ── 1. No publisher / outbox wiring of any kind (AL-INV-10). ───────────────
hits=$(scan 'NewSNSPublisher|SNSConfig\b|SNSConfigFromEnv|LoadSNS\(|LoadOutbox\(|RunnerConfigFromEnv|\bWithCodec\(')
[ -z "$hits" ] || fail "AL-INV-10: SNS publisher / publisher-config wiring detected. iam-audit-log publishes nothing." "$hits"

hits=$(scan '"github\.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/outbox"|\boutbox\.')
[ -z "$hits" ] || fail "AL-INV-10: platform-events outbox referenced. There is no outbox in this service (no ApplySchema, no Runner, no Enqueue)." "$hits"

hits=$(grep -RIn -i 'outbox_events' --include='*.go' --include='*.sql' cmd/ internal/ 2>/dev/null | grep -v '_test\.go' | grep -vE '^[^:]+:[0-9]+:[[:space:]]*(//|--)' || true)
[ -z "$hits" ] || fail "AL-INV-10: an outbox_events table/reference exists." "$hits"

hits=$(scan '"github\.com/aws/aws-sdk-go-v2/service/sns"')
[ -z "$hits" ] || fail "AL-INV-10: raw aws-sdk-go-v2/service/sns imported." "$hits"

hits=$(scan '\.(Publish|PublishBatch)\(')
[ -z "$hits" ] || fail "AL-INV-10: a Publish/PublishBatch call exists." "$hits"

# ── 2. SQS only via platform-events' consumer (O&M check #1/#2). ───────────
hits=$(grep -RIl -E '"github\.com/aws/aws-sdk-go-v2/service/sqs"' --include='*.go' cmd/ internal/ pkg/ 2>/dev/null | grep -v '_test\.go$' | grep -v '^cmd/server/main\.go$' || true)
[ -z "$hits" ] || fail "aws-sdk-go-v2/service/sqs imported outside cmd/server/main.go (consume via events.NewSQSConsumerWithClient only)." "$hits"

hits=$(scan '\.(SendMessage|ReceiveMessage|DeleteMessage|ChangeMessageVisibility|Subscribe)\(')
[ -z "$hits" ] || fail "Direct SQS transport call detected — the platform-events consumer owns receive/delete/visibility." "$hits"

# ── 3. No hand-built envelopes (O&M check #3). ─────────────────────────────
hits=$(scan 'events\.Envelope(\[[^]]*\])?\{')
[ -z "$hits" ] || fail "events.Envelope constructed as a struct literal." "$hits"

echo "AL-INV-10 check passed — no publisher, outbox, or raw SNS/SQS transport wiring found."
