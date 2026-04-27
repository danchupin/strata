#!/usr/bin/env bash
# s3cmd auto-multiparts above --multipart-chunk-size-mb (default 15 MB).
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
. "$HERE/_config.sh"
trap 'rm -f "$S3CFG"' EXIT

BUCKET="ex-mp-$(strata_suffix)"
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"; rm -f "$S3CFG"' EXIT

$S3CMD mb "s3://$BUCKET"
dd if=/dev/urandom of="$TMP/big.bin" bs=1M count=25 2>/dev/null
ORIG=$(md5of "$TMP/big.bin")

$S3CMD --multipart-chunk-size-mb=5 put "$TMP/big.bin" "s3://$BUCKET/big.bin"
$S3CMD get "s3://$BUCKET/big.bin" "$TMP/got.bin" --force >/dev/null
GOT=$(md5of "$TMP/got.bin")
[ "$ORIG" = "$GOT" ] || { echo "MISMATCH $ORIG vs $GOT"; exit 1; }
echo "  md5 ok ($GOT)"

$S3CMD del "s3://$BUCKET/big.bin"
$S3CMD rb "s3://$BUCKET"
echo "OK"
