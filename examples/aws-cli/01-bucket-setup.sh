#!/usr/bin/env bash
# Create a bucket, list buckets, head it, then delete it.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
. "$HERE/../lib/common.sh"

BUCKET="ex-bkt-$(strata_suffix)"

echo "== aws-cli: create-bucket $BUCKET"
aws_strata s3api create-bucket --bucket "$BUCKET" >/dev/null

echo "== aws-cli: list-buckets"
aws_strata s3api list-buckets --query 'Buckets[?Name==`'"$BUCKET"'`].Name' --output text

echo "== aws-cli: head-bucket"
aws_strata s3api head-bucket --bucket "$BUCKET"

echo "== aws-cli: delete-bucket"
aws_strata s3api delete-bucket --bucket "$BUCKET"
echo "OK"
