#!/usr/bin/env bash
# mc auto-multiparts above ~64 MiB. Upload 70 MiB to ensure the multipart
# code path runs end-to-end, then verify md5 round-trip.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
. "$HERE/_alias.sh"

BUCKET="ex-mp-$(strata_suffix)"
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT

mc mb "$MC_ALIAS/$BUCKET"
dd if=/dev/urandom of="$TMP/big.bin" bs=1M count=70 2>/dev/null
ORIG=$(md5of "$TMP/big.bin")

mc cp "$TMP/big.bin" "$MC_ALIAS/$BUCKET/big.bin"
mc cp "$MC_ALIAS/$BUCKET/big.bin" "$TMP/got.bin"
GOT=$(md5of "$TMP/got.bin")
[ "$ORIG" = "$GOT" ] || { echo "MISMATCH $ORIG vs $GOT"; exit 1; }
echo "  md5 ok ($GOT)"

mc rm "$MC_ALIAS/$BUCKET/big.bin"
mc rb "$MC_ALIAS/$BUCKET"
echo "OK"
