#!/usr/bin/env bash
# PUT with SSE-S3 (AES256), HEAD echoes the algo, GET round-trips plaintext.
# Requires gateway started with STRATA_SSE_MASTER_KEY set.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
. "$HERE/../lib/common.sh"

BUCKET="ex-sse-$(strata_suffix)"
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT

aws_strata s3api create-bucket --bucket "$BUCKET" >/dev/null
echo "encrypted secret $(date)" > "$TMP/secret.txt"

echo "== aws-cli: put-object with SSE-S3"
aws_strata s3api put-object --bucket "$BUCKET" --key secret.txt \
    --body "$TMP/secret.txt" --server-side-encryption AES256

echo "== aws-cli: head-object expects SSE echo"
ALGO="$(aws_strata s3api head-object --bucket "$BUCKET" --key secret.txt \
    --query ServerSideEncryption --output text)"
[ "$ALGO" = "AES256" ] || { echo "expected SSE=AES256, got '$ALGO'"; exit 1; }
echo "  SSE=$ALGO"

echo "== aws-cli: get-object decrypts transparently"
aws_strata s3api get-object --bucket "$BUCKET" --key secret.txt "$TMP/got.txt" >/dev/null
diff "$TMP/secret.txt" "$TMP/got.txt"
echo "  plaintext round-trip ok"

aws_strata s3api delete-object --bucket "$BUCKET" --key secret.txt >/dev/null
aws_strata s3api delete-bucket --bucket "$BUCKET"
echo "OK"
