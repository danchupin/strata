#!/usr/bin/env bash
# Drive `bin/strata-racecheck` against a freshly brought-up stack.
#
# Behaviour:
#   - In CI (CI=true), brings up the trimmed `ci` profile via `make up-all-ci`.
#     Otherwise uses the full developer stack via `make up-all`.
#   - Polls /readyz with a 10-minute ceiling (matches US-005's smoke timeout).
#   - Runs strata-racecheck for RACE_DURATION (default 1h) at RACE_CONCURRENCY
#     (default 32). The harness itself caps --concurrency at 64 and rejects
#     anything higher with exit code 2.
#   - Captures pre/post `df -h /` + `free -m` into report/host.txt and the
#     per-container docker logs for strata, strata-cassandra, strata-ceph
#     into report/.
#   - Exits with the harness's exit code (0 clean, 1 inconsistencies,
#     2 transport / setup error).
#
# Idempotent: re-running clears the prior report/ directory so each invocation
# produces a fresh snapshot.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$REPO_ROOT"

REPORT_DIR="${REPORT_DIR:-report}"
RACE_DURATION="${RACE_DURATION:-1h}"
RACE_CONCURRENCY="${RACE_CONCURRENCY:-32}"
RACE_BUCKETS="${RACE_BUCKETS:-4}"
RACE_KEYS_PER_BUCKET="${RACE_KEYS_PER_BUCKET:-32}"
RACE_ENDPOINT="${RACE_ENDPOINT:-http://localhost:9999}"
RACE_ACCESS_KEY="${RACE_ACCESS_KEY:-${AWS_ACCESS_KEY_ID:-strataAK}}"
RACE_SECRET_KEY="${RACE_SECRET_KEY:-${AWS_SECRET_ACCESS_KEY:-strataSK}}"
RACE_REGION="${RACE_REGION:-${AWS_DEFAULT_REGION:-us-east-1}}"
READYZ_TIMEOUT_SEC="${READYZ_TIMEOUT_SEC:-600}"

CONTAINERS=(strata strata-cassandra strata-ceph)

log() { printf '[race-soak] %s\n' "$*"; }

snapshot_host() {
  local label="$1"
  {
    echo "== ${label} $(date -u +%FT%TZ)"
    echo "-- df -h /"
    df -h / || true
    echo "-- free -m"
    if command -v free >/dev/null 2>&1; then
      free -m || true
    else
      echo "free(1) unavailable on $(uname -s)"
    fi
    echo
  } >>"$REPORT_DIR/host.txt"
}

dump_container_logs() {
  for c in "${CONTAINERS[@]}"; do
    log "capturing docker logs for $c"
    docker logs "$c" >"$REPORT_DIR/${c}.log" 2>&1 || \
      log "  warning: docker logs $c failed (container may not be up)"
  done
}

# Idempotency: scrub prior report/ directory so we start clean.
log "clearing $REPORT_DIR"
rm -rf "$REPORT_DIR"
mkdir -p "$REPORT_DIR"

# Build binary if missing.
if [[ ! -x bin/strata-racecheck ]]; then
  log "building bin/strata-racecheck"
  go build -o bin/strata-racecheck ./cmd/strata-racecheck
fi

# Bring up the stack. Operator may opt out (STACK_UP=0) when the gateway
# is already running, e.g. local iteration.
if [[ "${STACK_UP:-1}" == "1" ]]; then
  if [[ "${CI:-false}" == "true" ]]; then
    log "make up-all-ci"
    make up-all-ci
  else
    log "make up-all"
    make up-all
  fi
fi

snapshot_host "pre-readyz"

log "waiting for /readyz on ${RACE_ENDPOINT} (ceiling ${READYZ_TIMEOUT_SEC}s)"
deadline=$(( $(date +%s) + READYZ_TIMEOUT_SEC ))
while :; do
  code="$(curl -fsS -o /dev/null -w '%{http_code}' "${RACE_ENDPOINT}/readyz" || true)"
  if [[ "$code" == "200" ]]; then
    log "gateway ready"
    break
  fi
  if (( $(date +%s) >= deadline )); then
    log "readyz timeout — capturing logs and exiting 2"
    dump_container_logs
    snapshot_host "post-failure"
    exit 2
  fi
  sleep 2
done

snapshot_host "pre-load"

log "running strata-racecheck duration=${RACE_DURATION} concurrency=${RACE_CONCURRENCY}"
set +e
./bin/strata-racecheck \
  --endpoint="$RACE_ENDPOINT" \
  --duration="$RACE_DURATION" \
  --concurrency="$RACE_CONCURRENCY" \
  --buckets="$RACE_BUCKETS" \
  --keys-per-bucket="$RACE_KEYS_PER_BUCKET" \
  --report="$REPORT_DIR/race.jsonl" \
  --access-key="$RACE_ACCESS_KEY" \
  --secret-key="$RACE_SECRET_KEY" \
  --region="$RACE_REGION" \
  | tee "$REPORT_DIR/race.stdout.log"
HARNESS_EXIT="${PIPESTATUS[0]}"
set -e

snapshot_host "post-load"
dump_container_logs

log "harness exit code: $HARNESS_EXIT"
exit "$HARNESS_EXIT"
