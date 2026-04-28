#!/usr/bin/env bash
# Drives a single PutObject through a strata gateway whose notify worker is
# wired to a webhook receiver, then asserts the receiver saw the event.
# Used by CI's e2e-full job (compose `features` profile + STRATA_NOTIFY_TARGETS
# pointing at the `webhook-trap` service).
set -euo pipefail

BASE="${1:-http://127.0.0.1:9999}"
TRAP_CONTAINER="${TRAP_CONTAINER:-strata-webhook-trap}"
TOPIC_ARN="${TOPIC_ARN:-arn:aws:sns:us-east-1:000:test}"
WAIT_SECS="${WAIT_SECS:-90}"

export AWS_ACCESS_KEY_ID="${AWS_ACCESS_KEY_ID:-strataAK}"
export AWS_SECRET_ACCESS_KEY="${AWS_SECRET_ACCESS_KEY:-strataSK}"
export AWS_DEFAULT_REGION="${AWS_DEFAULT_REGION:-us-east-1}"
export AWS_EC2_METADATA_DISABLED=true

aws_cmd="aws --endpoint-url=$BASE --no-verify-ssl"

BUCKET="notify-$(date +%s)"
KEY="hello-notify-$(date +%s).txt"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

echo "== create bucket $BUCKET"
$aws_cmd s3api create-bucket --bucket "$BUCKET" >/dev/null

echo "== put-bucket-notification-configuration"
cat > "$TMP/notify.json" <<EOF
{
  "TopicConfigurations": [
    {
      "Id": "ci-test",
      "TopicArn": "$TOPIC_ARN",
      "Events": ["s3:ObjectCreated:*"]
    }
  ]
}
EOF
$aws_cmd s3api put-bucket-notification-configuration \
  --bucket "$BUCKET" \
  --notification-configuration "file://$TMP/notify.json" >/dev/null

echo "== PUT object $KEY"
echo "hello notify $(date)" > "$TMP/payload"
$aws_cmd s3 cp "$TMP/payload" "s3://$BUCKET/$KEY" >/dev/null

echo "== await webhook delivery (up to ${WAIT_SECS}s, container=$TRAP_CONTAINER)"
for i in $(seq 1 "$WAIT_SECS"); do
  if docker logs "$TRAP_CONTAINER" 2>&1 | grep -q "$KEY"; then
    echo "  webhook received event referencing $KEY (after ${i}s)"
    docker logs "$TRAP_CONTAINER" 2>&1 | grep -E "ObjectCreated|$KEY" | head -5 || true
    exit 0
  fi
  sleep 1
done

echo "FAIL — webhook never received event for $KEY"
echo "---- recent webhook-trap logs ----"
docker logs "$TRAP_CONTAINER" 2>&1 | tail -100 || true
echo "---- recent strata logs ----"
docker logs strata 2>&1 | tail -100 || true
exit 1
