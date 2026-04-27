#!/usr/bin/env bash
# Set, get, delete a lifecycle policy.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
. "$HERE/../lib/common.sh"

BUCKET="ex-lc-$(strata_suffix)"
aws_strata s3api create-bucket --bucket "$BUCKET" >/dev/null

echo "== aws-cli: put-bucket-lifecycle-configuration"
aws_strata s3api put-bucket-lifecycle-configuration \
    --bucket "$BUCKET" \
    --lifecycle-configuration "file://$HERE/lifecycle.json"

echo "== aws-cli: get-bucket-lifecycle-configuration"
aws_strata s3api get-bucket-lifecycle-configuration --bucket "$BUCKET"

echo "== aws-cli: delete-bucket-lifecycle"
aws_strata s3api delete-bucket-lifecycle --bucket "$BUCKET"

aws_strata s3api delete-bucket --bucket "$BUCKET"
echo "OK"
