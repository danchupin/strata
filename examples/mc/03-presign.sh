#!/usr/bin/env bash
# `mc share download` is the mc equivalent of presigned-GET.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
. "$HERE/_alias.sh"

BUCKET="ex-pre-$(strata_suffix)"
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT

mc mb "$MC_ALIAS/$BUCKET"
echo "presigned content $(date)" > "$TMP/secret.txt"
mc cp "$TMP/secret.txt" "$MC_ALIAS/$BUCKET/secret.txt"

URL="$(mc share download --expire 1m "$MC_ALIAS/$BUCKET/secret.txt" \
    | awk '/Share:/ {print $2}')"
[ -n "$URL" ] || { echo "no share URL produced"; mc share download --expire 1m "$MC_ALIAS/$BUCKET/secret.txt"; exit 1; }
echo "presigned: $URL"

GOT="$(env -i PATH="$PATH" curl -sf "$URL")"
[ "$GOT" = "$(cat "$TMP/secret.txt")" ] || { echo "presigned mismatch"; exit 1; }
echo "  ok"

mc rm "$MC_ALIAS/$BUCKET/secret.txt"
mc rb "$MC_ALIAS/$BUCKET"
echo "OK"
