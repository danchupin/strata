#!/usr/bin/env bash
# 25 MiB upload exercises aws-cli's automatic multipart path. Verify md5 round-trip.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
. "$HERE/../lib/common.sh"

BUCKET="ex-mp-$(strata_suffix)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

aws_strata s3api create-bucket --bucket "$BUCKET" >/dev/null

dd if=/dev/urandom of="$TMP/big.bin" bs=1M count=25 2>/dev/null
ORIG=$(md5of "$TMP/big.bin")

echo "== aws-cli: upload 25 MiB (multipart)"
aws_strata s3 cp "$TMP/big.bin" "s3://$BUCKET/big.bin" >/dev/null

echo "== aws-cli: list parts via list-objects-v2"
aws_strata s3api list-objects-v2 --bucket "$BUCKET" --query 'Contents[].[Key,Size]' --output text

echo "== aws-cli: download + verify md5"
aws_strata s3 cp "s3://$BUCKET/big.bin" "$TMP/big.got" >/dev/null
GOT=$(md5of "$TMP/big.got")
[ "$ORIG" = "$GOT" ] || { echo "MISMATCH $ORIG vs $GOT"; exit 1; }
echo "  md5 ok ($GOT)"

aws_strata s3 rm "s3://$BUCKET/big.bin" >/dev/null
aws_strata s3api delete-bucket --bucket "$BUCKET"
echo "OK"
