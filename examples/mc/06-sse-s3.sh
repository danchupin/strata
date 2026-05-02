#!/usr/bin/env bash
# mc supports SSE-S3 via `--enc-s3` (or `mc encrypt set sse-s3` per bucket).
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
. "$HERE/_alias.sh"

BUCKET="ex-sse-$(strata_suffix)"
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT

mc mb "$MC_ALIAS/$BUCKET"
echo "encrypted secret $(date)" > "$TMP/secret.txt"

# Per-PUT SSE-S3:
mc cp --enc-s3="$MC_ALIAS/$BUCKET/secret.txt" "$TMP/secret.txt" "$MC_ALIAS/$BUCKET/secret.txt"

# HEAD via mc stat -- look for the SSE field.
ALGO="$(mc stat --json "$MC_ALIAS/$BUCKET/secret.txt" \
    | python3 -c 'import json,sys; d=json.load(sys.stdin); m=d.get("metadata",{}); print(m.get("X-Amz-Server-Side-Encryption",""))')"
[ "$ALGO" = "AES256" ] || { echo "expected SSE=AES256, got '$ALGO'"; exit 1; }
echo "  SSE=$ALGO"

mc cp "$MC_ALIAS/$BUCKET/secret.txt" "$TMP/got.txt"
diff "$TMP/secret.txt" "$TMP/got.txt"
echo "  plaintext round-trip ok"

mc rm "$MC_ALIAS/$BUCKET/secret.txt"
mc rb "$MC_ALIAS/$BUCKET"
echo "OK"
