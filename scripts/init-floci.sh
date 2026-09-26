#!/usr/bin/env bash
# Floci ready-hook — provisions iam-audit-log's local AWS topology
# (LLD §7.1, §7.3.1, §15.4). Mounted at /etc/floci/init/ready.d/. Floci is
# the LocalStack-compatible emulator every sibling IAM service uses (the
# LLD's "init-localstack.sh" — same role, sibling tooling).
#
# Creates:
#   11 SNS topics      — the consumed topics (owned by other services in
#                        production; created here so local dev can publish)
#   11 SQS queues+DLQs — <topic-short>-audit-q / -dlq, maxReceiveCount=5,
#                        each subscribed catch-all (no filter, AL-EVT-2)
#                        with RawMessageDelivery=true
#   1 S3 bucket        — iam-audit-archive, Object Lock enabled (§15.4)
#   6 Glue registries  — iam-auth-events … iam-serviceaccount-events,
#                        read-only for this service (it registers nothing);
#                        created LAST so the compose healthcheck (which polls
#                        the last registry) signals full provisioning.
set -euo pipefail

AWS_REGION=ap-south-1
BUCKET=iam-audit-archive
export AWS_DEFAULT_REGION="$AWS_REGION"
export AWS_REGION="$AWS_REGION"

echo "[init-floci] provisioning iam-audit-log topology..."

create_queue_with_dlq() {
  local base="$1" dlq_url dlq_arn queue_url attrs_file
  dlq_url=$(aws sqs create-queue --queue-name "${base}-dlq" --query QueueUrl --output text)
  dlq_arn=$(aws sqs get-queue-attributes --queue-url "$dlq_url" --attribute-names QueueArn --query Attributes.QueueArn --output text)
  attrs_file=$(mktemp)
  cat > "$attrs_file" <<JSON
{"RedrivePolicy": "{\"deadLetterTargetArn\":\"$dlq_arn\",\"maxReceiveCount\":\"5\"}"}
JSON
  queue_url=$(aws sqs create-queue --queue-name "$base" --attributes "file://$attrs_file" --query QueueUrl --output text)
  rm -f "$attrs_file"
  echo "$queue_url"
}

subscribe_queue() {
  local topic_arn="$1" queue_url="$2" queue_arn sub_arn
  queue_arn=$(aws sqs get-queue-attributes --queue-url "$queue_url" --attribute-names QueueArn --query Attributes.QueueArn --output text)
  sub_arn=$(aws sns subscribe --topic-arn "$topic_arn" --protocol sqs --notification-endpoint "$queue_arn" --query SubscriptionArn --output text)
  # RawMessageDelivery=true is required by platform-events' consumer.
  aws sns set-subscription-attributes --subscription-arn "$sub_arn" --attribute-name RawMessageDelivery --attribute-value true >/dev/null
}

# topic (SNS name) : queue : env var
PAIRS="
iam-auth-events:auth-audit-q:AUTH_AUDIT_QUEUE_URL
iam-user-events:user-audit-q:USER_AUDIT_QUEUE_URL
iam-membership-events:membership-audit-q:MEMBERSHIP_AUDIT_QUEUE_URL
iam-tenant-events:tenant-audit-q:TENANT_AUDIT_QUEUE_URL
iam-delegation-events:delegation-audit-q:DELEGATION_AUDIT_QUEUE_URL
iam-serviceaccount-events:serviceaccount-audit-q:SERVICEACCOUNT_AUDIT_QUEUE_URL
tender-events:tender-audit-q:TENDER_AUDIT_QUEUE_URL
billing-events:billing-audit-q:BILLING_AUDIT_QUEUE_URL
usage-events:usage-audit-q:USAGE_AUDIT_QUEUE_URL
wf-workflow-events:wf-workflow-audit-q:WF_WORKFLOW_AUDIT_QUEUE_URL
wf-template-events:wf-template-audit-q:WF_TEMPLATE_AUDIT_QUEUE_URL
"

summary=""
for pair in $PAIRS; do
  topic="${pair%%:*}"; rest="${pair#*:}"; queue="${rest%%:*}"; envvar="${rest#*:}"
  topic_arn=$(aws sns create-topic --name "$topic" --query TopicArn --output text)
  queue_url=$(create_queue_with_dlq "$queue")
  subscribe_queue "$topic_arn" "$queue_url"
  summary="${summary}  ${envvar}=${queue_url}\n"
done

aws s3api create-bucket --bucket "$BUCKET" \
  --create-bucket-configuration LocationConstraint="$AWS_REGION" \
  --object-lock-enabled-for-bucket >/dev/null
echo "S3 bucket: $BUCKET (Object Lock enabled)"

for reg in iam-auth-events iam-user-events iam-membership-events iam-tenant-events iam-delegation-events iam-serviceaccount-events; do
  aws glue create-registry --registry-name "$reg" >/dev/null
done
echo "Glue registries created (read-only for this service)"

echo ""
echo "[init-floci] done."
printf '%b' "$summary"
echo "  AUDIT_ARCHIVE_BUCKET=$BUCKET"
