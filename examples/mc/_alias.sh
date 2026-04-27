# Sourced helper: register an mc alias pointing at the running gateway.
# Defines $MC_ALIAS for callers and ensures `mc alias set` succeeded.
HERE="$(cd "$(dirname "$0")" && pwd)"
. "$HERE/../lib/common.sh"

MC_ALIAS="strata-examples"
MC_BIN="${MC:-mc}"

# api S3v4 + path style + insecure (HTTP) for an in-memory gateway.
"$MC_BIN" alias set "$MC_ALIAS" \
    "$STRATA_ENDPOINT" "$STRATA_ACCESS_KEY" "$STRATA_SECRET_KEY" \
    --api S3v4 --path on >/dev/null
