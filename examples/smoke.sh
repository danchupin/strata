#!/usr/bin/env bash
# Boot a fresh in-memory Strata gateway and run every example script
# end-to-end. Exits 0 on success, non-zero on first failure.
#
# - aws-cli scripts are required.
# - boto3 / mc / s3cmd scripts are skipped per-tool when unavailable.
#
# All gateway state lives in $TMPDIR/strata-examples-* and is cleaned up
# on exit.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")"/.. && pwd)"
HERE="$ROOT/examples"
. "$HERE/lib/common.sh"

if ! command -v aws >/dev/null 2>&1; then
    echo "fail: aws-cli is required" >&2
    exit 1
fi

# A free port for the gateway. 0=>kernel-picked.
PORT="${STRATA_EXAMPLES_PORT:-19999}"
export STRATA_ENDPOINT="http://127.0.0.1:$PORT"

# Static creds for the [iam root] principal -- third field == owner.
# Secret length >= 8 chars to satisfy mc's MinIO-compat key validator.
export STRATA_ACCESS_KEY="adminAccessKey"
export STRATA_SECRET_KEY="adminSecretKey"
export STRATA_REGION="us-east-1"

# Generate a 32-byte master key for SSE-S3 (64 hex chars).
# `set -o pipefail` would trip on SIGPIPE from `tr` here, so build the key
# in a subshell with a relaxed shell flagset.
SSE_KEY="$(set +o pipefail; env LC_ALL=C tr -dc 'a-f0-9' </dev/urandom | head -c 64)"

WORK="$(mktemp -d -t strata-examples.XXXXXX)"
LOG="$WORK/gateway.log"
PIDFILE="$WORK/gateway.pid"

cleanup() {
    if [ -f "$PIDFILE" ]; then
        local pid; pid="$(cat "$PIDFILE")"
        kill -TERM "$pid" 2>/dev/null || true
        wait "$pid" 2>/dev/null || true
    fi
    rm -rf "$WORK"
}
trap cleanup EXIT INT TERM

echo "== Booting strata-gateway on $STRATA_ENDPOINT (logs: $LOG)"
(
    cd "$ROOT"
    STRATA_LISTEN=":$PORT" \
    STRATA_META_BACKEND=memory \
    STRATA_DATA_BACKEND=memory \
    STRATA_AUTH_MODE=required \
    STRATA_STATIC_CREDENTIALS="$STRATA_ACCESS_KEY:$STRATA_SECRET_KEY:iam-root" \
    STRATA_SSE_MASTER_KEY="$SSE_KEY" \
    STRATA_LOG_LEVEL=WARN \
    go run ./cmd/strata-gateway >"$LOG" 2>&1 &
    echo $! > "$PIDFILE"
)
strata_wait_ready

run_dir() {
    local dir="$1"
    local glob="$2"
    local label="$3"
    echo
    echo "================ $label ================"
    local script
    for script in "$HERE/$dir"/$glob; do
        [ -e "$script" ] || continue
        echo
        echo "---- $(basename "$script") ----"
        if [[ "$script" == *.py ]]; then
            (cd "$HERE/$dir" && python3 "$(basename "$script")")
        else
            bash "$script"
        fi
    done
}

run_dir aws-cli '0[1-7]-*.sh' "aws-cli"

if python3 -c 'import boto3' >/dev/null 2>&1; then
    run_dir boto3 '0[1-7]-*.py' "boto3"
else
    echo
    echo "skip boto3: 'python3 -c \"import boto3\"' failed (pip install boto3)"
fi

if command -v mc >/dev/null 2>&1; then
    run_dir mc '0[1-7]-*.sh' "mc"
else
    echo
    echo "skip mc: 'mc' binary not found"
fi

if command -v s3cmd >/dev/null 2>&1; then
    run_dir s3cmd '0[1-7]-*.sh' "s3cmd"
else
    echo
    echo "skip s3cmd: 's3cmd' binary not found"
fi

echo
echo "================ ALL OK ================"
