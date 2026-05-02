#!/usr/bin/env bash
# `--server-side-encryption` enables SSE-S3 on PUT.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
. "$HERE/_config.sh"
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"; rm -f "$S3CFG"' EXIT

BUCKET="ex-sse-$(strata_suffix)"
$S3CMD mb "s3://$BUCKET"
echo "encrypted secret $(date)" > "$TMP/secret.txt"

$S3CMD --server-side-encryption put "$TMP/secret.txt" "s3://$BUCKET/secret.txt"

ALGO="$($S3CMD info "s3://$BUCKET/secret.txt" | awk -F': +' '/^[[:space:]]*SSE:/ {print $2}')"
[ "$ALGO" = "AES256" ] || { echo "expected SSE=AES256, got '$ALGO'"; exit 1; }
echo "  SSE=$ALGO"

$S3CMD get "s3://$BUCKET/secret.txt" "$TMP/got.txt" --force >/dev/null
diff "$TMP/secret.txt" "$TMP/got.txt"
echo "  plaintext round-trip ok"

$S3CMD del "s3://$BUCKET/secret.txt"
$S3CMD rb "s3://$BUCKET"
echo "OK"
