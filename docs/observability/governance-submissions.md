# Observability Governance Submissions — iam-audit-log

Formal registry-ratification submissions required by the Enterprise Platform
Observability Standard: "Any proposed platform-shared metric that is not already
present in the Platform Observability Registry MUST be submitted for approval …
along with its semantic definition, required labels, allowed label values, and
aggregation expectations before broad adoption."

This file documents every `proposed` Tier-1 and Tier-2 metric this service
depends on. When governance ratifies a metric, flip its status to
`StatusCanonical` in `internal/adapter/outbound/metrics/registry.go` and in
`deploy/monitoring/metric-registry.yaml`, drop the `metric_status: proposed`
markers from alerts/recording-rules/SLOs/dashboards/HPA, and update the status
below. `TestRegistryLedger_MatchesPlatformRegistry` fails until `registry.go`
and the platform-gincommon obsregistry agree.

---

## Tier 1 — Platform Shared Metrics (proposed)

### `platform_duplicate_messages_total`

| Field | Value |
|---|---|
| **Semantic definition** | A message redelivered by SQS and acknowledged without running its handler because the consumer's dedup ledger had already recorded its envelope ID. Counts at-least-once redeliveries that the idempotency layer absorbs. One increment per duplicate delivery, after the dedup check succeeds. |
| **Required labels** | `domain`, `service`, `environment` (centrally injected by gincommon) |
| **Approved dimension labels** | `queue` (the inbound SQS queue name), `event_type` (the envelope's event type, bounded to the taxonomy vocabulary; `unknown` for unrecognised types) |
| **Forbidden labels** | Any high-cardinality value: `tenant_id`, `user_id`, `event_id`, `request_id`, `session_id` |
| **Allowed label values** | `queue`: 12 bounded values (see `label_vocabulary.queue` in `metric-registry.yaml`); `event_type`: bounded taxonomy vocabulary |
| **Aggregation expectations** | `sum by (domain, service, environment, queue)` for queue-level dedup rate; `sum by (domain, service, environment)` for fleet-wide dedup rate. Never aggregate by `tenant_id` or `user_id`. |
| **Why platform_* (not iam_*)** | Dedup of redelivered messages is semantically identical across IAM, Billing, Workflow and any other SQS-consuming service. Any service that uses platform-events' at-least-once consumption with a processed_events ledger can emit the same metric with identical semantics. |
| **Who emits it here** | `internal/adapter/outbound/metrics/business.go` (`DuplicateMessages`), built with `gincommon.NewSharedCounterVec`. platform-events adopts the same collector (same descriptor); both record into one series. |
| **Current status** | `proposed` — awaiting Platform Observability Registry ratification |

---

### `platform_dependency_request_seconds`

| Field | Value |
|---|---|
| **Semantic definition** | Latency of a synchronous outbound call to a named dependency, measured from the start of the network call to its completion (success or error). The histogram's `_sum`/`_count` yield the mean; `_bucket` yields quantiles. One observation per call. |
| **Required labels** | `domain`, `service`, `environment` (centrally injected by gincommon) |
| **Approved dimension labels** | `dependency` (name of the called system), `operation` (specific operation on that system), `outcome` (`success`, `failure`, `error`) |
| **Forbidden labels** | `tenant_id`, `user_id`, `request_id`, `trace_id`, `event_id` |
| **Allowed label values** | `dependency`: `catalog-admin`, `s3` (service-recorded); `sqs`, `codec` (platform-events). `operation`: `list_plans`, `read_archive`, `put_archive`, `verify_archive`, `put_export`, `consume`. `outcome`: `success`, `failure`, `error` |
| **Aggregation expectations** | `histogram_quantile(0.99, rate(platform_dependency_request_seconds_bucket[5m]))` by `dependency`/`operation`; error ratio via `outcome="error"` share of `_count`. Never aggregate by `tenant_id`. |
| **Why platform_* (not iam_*)** | Every service that makes synchronous outbound calls (to databases, object stores, downstream APIs) can observe them with the same `dependency`/`operation`/`outcome` semantics. Billing, Workflow and Tender Management all call S3 or external APIs. |
| **Who emits it here** | `business.go` (`DependencyRequestSeconds`) for `catalog-admin` and `s3` calls; platform-events for `sqs` and `codec`. Both adopt the same collector. |
| **Current status** | `proposed` — awaiting Platform Observability Registry ratification |

---

### `platform_queue_depth`

| Field | Value |
|---|---|
| **Semantic definition** | The approximate number of messages visible in the inbound SQS queue at the moment of the sample. Set as a gauge from `ApproximateNumberOfMessages`. Increases indicate consumer lag or traffic spikes; decreases indicate recovery. |
| **Required labels** | `domain`, `service`, `environment` (centrally injected by gincommon) |
| **Approved dimension labels** | `queue` (the inbound SQS queue name) |
| **Forbidden labels** | `tenant_id`, `user_id`, `event_id` |
| **Allowed label values** | `queue`: bounded to the 12 `*-audit-q` values (see `label_vocabulary.queue`) |
| **Aggregation expectations** | `max by (queue)` for the per-queue watermark; `sum by (domain, service, environment)` for the fleet-wide backlog. Used as an HPA external metric: `sum(platform_queue_depth{service="iam-audit-log"})`. |
| **Why platform_* (not iam_*)** | Queue depth is a universal SQS-consumer concept. Billing, Workflow and every other event-driven service has an inbound queue whose depth matters for capacity decisions. The `queue` label carries all needed differentiation. |
| **Who emits it here** | `cmd/server`'s `OpsMonitor`, writing `metrics.QueueDepth`, which is built with `gincommon.NewSharedGaugeVec`. |
| **Current status** | `proposed` — awaiting Platform Observability Registry ratification |

---

### `platform_dlq_depth`

| Field | Value |
|---|---|
| **Semantic definition** | The approximate number of messages in the dead-letter queue for one inbound SQS queue at the moment of the sample. Any value above 0 is a compliance incident for this service (a DLQ'd audit event is an audit gap). |
| **Required labels** | `domain`, `service`, `environment` (centrally injected by gincommon) |
| **Approved dimension labels** | `queue` (the **source** queue's name, not the DLQ's name, so aggregation aligns with `platform_messages_*` and `platform_queue_depth`) |
| **Forbidden labels** | `tenant_id`, `user_id`, `event_id` |
| **Allowed label values** | `queue`: same 12-value vocabulary as `platform_queue_depth` |
| **Aggregation expectations** | `max by (queue)` for per-queue DLQ watermark; `sum by (domain, service, environment) > 0` as the SLO error condition. Alert: `IAMAuditLogDLQNotEmpty` fires when any queue's DLQ depth exceeds 0. |
| **Why platform_* (not iam_*)** | DLQ depth is meaningful and actionable for every SQS-consuming service across all domains. The semantics are identical regardless of what domain uses SQS. |
| **Who emits it here** | `cmd/server`'s `OpsMonitor`, writing `metrics.DLQDepth`, built with `gincommon.NewSharedGaugeVec`. |
| **Current status** | `proposed` — awaiting Platform Observability Registry ratification |

---

## Tier 2 — IAM Domain Shared Metrics (proposed)

### `iam_rls_violations_total`

| Field | Value |
|---|---|
| **Semantic definition** | A PostgreSQL Row-Level Security check failure detected by the `rls_violation_log` trigger and counted fleet-wide by the `audit_rls_violation_counts()` SECURITY DEFINER function (migration `000010`). One increment per violation row consumed, with a watermark preventing double-counting across replicas. |
| **Required labels** | `domain`, `service`, `environment` (centrally injected by gincommon) |
| **Approved dimension labels** | `violation_type` (class of violation: `cross_tenant_access`, `missing_or_invalid_guc`, `other`) |
| **Forbidden labels** | `tenant_id`, `user_id`, `query`, `table` |
| **Allowed label values** | `violation_type`: `cross_tenant_access` (a query ran against a different tenant's data), `missing_or_invalid_guc` (the `app.tenant_id` GUC was absent or non-UUID when RLS checked it), `other` (any other violation class logged by the trigger) |
| **Aggregation expectations** | `sum by (service, environment, violation_type)` for per-service violation counts; `sum by (domain, environment, violation_type)` for the IAM-wide RLS health view. Alert threshold: any `cross_tenant_access` is Critical. |
| **Why iam_* (not platform_*)** | RLS-based tenant isolation is an IAM architectural pattern (platform-pgcommon's RLS helpers, the `app.tenant_id` GUC, `FORCE ROW LEVEL SECURITY`). Billing and Workflow do not use the same RLS model. iam-org-membership and iam-user-profile emit the same metric with identical semantics (established by iam-user-profile's LLD §11.2). |
| **Who emits it here** | `business.go` (`RLSViolations`), built with `gincommon.NewSharedCounterVec` from the IAM Domain Registry entry. |
| **Registry owner needed** | No IAM domain owner has claimed this metric in the central registry yet. Proposed owner: IAM platform team. |
| **Current status** | `proposed` — awaiting IAM Domain Observability Registry ratification and owner assignment |

---

## Already canonical (no submission needed)

| Metric | Canonical since |
|---|---|
| `platform_messages_received_total` | platform-events v2.0.0 |
| `platform_messages_processed_total` | platform-events v2.0.0 |
| `platform_messages_failed_total` | platform-events v2.0.0 |
| `platform_retry_total` | platform-events v2.0.0 |
| `platform_dlq_messages_total` | platform-events v2.0.0 |
| `platform_unclassified_errors_total` | platform-gincommon v2.0.0 |

---

## How to ratify a proposed metric

1. The service team (or platform team, for Tier 1) opens a PR against the
   `platform-gincommon` repository updating `obsregistry/registry.go` with
   the metric's name, help, label set, and buckets (histograms), and sets its
   status to `Canonical`.
2. After the PR merges and a new gincommon release ships, update:
   - `internal/adapter/outbound/metrics/registry.go`: flip `StatusProposed` → `StatusCanonical`
   - `deploy/monitoring/metric-registry.yaml`: flip `status: proposed` → `status: canonical`
   - Remove `metric_status: proposed` from every alert label that reads the metric
   - Remove `# metric_status: proposed` comments from recording rules
   - Remove `(Proposed)` from dashboard panel titles
   - Remove `observability.platform/metric-status: proposed` from HPA annotations
3. Run `make metrics-lint` and `make ci` to verify the markers are consistent.
   `TestRegistryLedger_MatchesPlatformRegistry` will fail until step 2 is complete.
