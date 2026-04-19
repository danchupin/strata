#!/usr/bin/env bash
set -euo pipefail

BASE="${1:-http://127.0.0.1:9999}"
AK="${AWS_ACCESS_KEY_ID:-strataAK}"
SK="${AWS_SECRET_ACCESS_KEY:-strataSK}"

export AWS_ACCESS_KEY_ID="$AK"
export AWS_SECRET_ACCESS_KEY="$SK"
export AWS_DEFAULT_REGION="${AWS_DEFAULT_REGION:-us-east-1}"
export AWS_EC2_METADATA_DISABLED=true

BUCKET="signed-$(date +%s)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

aws="aws --endpoint-url=$BASE --no-verify-ssl"

md5of() {
  if command -v md5 >/dev/null 2>&1; then md5 -q "$1"; else md5sum "$1" | awk '{print $1}'; fi
}

echo "== unauthed request must be rejected"
code=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/" -X GET)
echo "  GET / (no auth) -> HTTP $code"
if [ "$code" != "403" ]; then echo "  FAIL — expected 403"; exit 1; fi

echo "== aws-cli GET buckets"
$aws s3api list-buckets >/dev/null && echo "  ok"

echo "== aws-cli create-bucket $BUCKET"
$aws s3api create-bucket --bucket "$BUCKET" >/dev/null && echo "  ok"

echo "== aws-cli put-object small"
echo "hello signed" > "$TMP/small.txt"
$aws s3 cp "$TMP/small.txt" "s3://$BUCKET/greeting.txt" >/dev/null && echo "  ok"

echo "== aws-cli get-object"
$aws s3 cp "s3://$BUCKET/greeting.txt" "$TMP/small.got" >/dev/null
[ "$(cat $TMP/small.got)" = "hello signed" ] && echo "  match"

echo "== aws-cli put-object 25 MiB (triggers multipart + streaming payload)"
dd if=/dev/urandom of="$TMP/big.bin" bs=1m count=25 2>/dev/null
ORIG=$(md5of "$TMP/big.bin")
$aws s3 cp "$TMP/big.bin" "s3://$BUCKET/big.bin" >/dev/null && echo "  uploaded"
$aws s3 cp "s3://$BUCKET/big.bin" "$TMP/big.got" >/dev/null
GOT=$(md5of "$TMP/big.got")
[ "$ORIG" = "$GOT" ] && echo "  md5 match ($GOT)" || { echo "  MISMATCH $ORIG vs $GOT"; exit 1; }

echo "== aws-cli list-objects-v2"
$aws s3api list-objects-v2 --bucket "$BUCKET" --query 'Contents[].Key' --output text

echo "== aws-cli delete objects + bucket"
$aws s3 rm "s3://$BUCKET/greeting.txt" >/dev/null
$aws s3 rm "s3://$BUCKET/big.bin" >/dev/null
$aws s3api delete-bucket --bucket "$BUCKET" >/dev/null
echo "  ok"

echo "== bad credentials rejected"
bad=$(AWS_ACCESS_KEY_ID=bogus AWS_SECRET_ACCESS_KEY=bogus aws --endpoint-url=$BASE s3api list-buckets 2>&1 || true)
if printf '%s' "$bad" | grep -qE 'status code: 403|InvalidAccessKeyId'; then
  echo "  ok"
else
  echo "  FAIL: $bad"
  exit 1
fi

echo "== signed smoke OK"
