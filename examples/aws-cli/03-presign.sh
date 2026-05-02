#!/usr/bin/env bash
# Generate a presigned GET URL and fetch via plain curl (no AWS creds in env).
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
. "$HERE/../lib/common.sh"

BUCKET="ex-pre-$(strata_suffix)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

aws_strata s3api create-bucket --bucket "$BUCKET" >/dev/null
echo "presigned content $(date)" > "$TMP/secret.txt"
aws_strata s3 cp "$TMP/secret.txt" "s3://$BUCKET/secret.txt" >/dev/null

echo "== aws-cli: presign GET"
URL="$(aws_strata s3 presign "s3://$BUCKET/secret.txt" --expires-in 60)"
echo "  $URL"

echo "== curl (no creds): GET presigned URL"
GOT="$(env -i PATH="$PATH" curl -sf "$URL")"
[ "$GOT" = "$(cat "$TMP/secret.txt")" ] || { echo "presigned download mismatch"; exit 1; }
echo "  ok"

aws_strata s3 rm "s3://$BUCKET/secret.txt" >/dev/null
aws_strata s3api delete-bucket --bucket "$BUCKET"
echo "OK"
