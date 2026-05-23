#!/usr/bin/env bash
# RGW lab restart smoke (US-006 of ralph/p1-fixes). Closes the loop on
# the US-005 lab-restart fix — without an explicit smoke harness future
# regressions only surface at ops time.
#
# Scenario:
#   Loop 3 iterations: `make up-all` → `make wait-tikv` + `make wait-ceph`
#   → `make up-bench-rgw` → `make wait-rgw` → `aws s3 ls` (empty) →
#   `make down`. Asserts no aborts across any iteration. Each iteration
#   logs `[iter N] <state>` lines so the run is greppable.
#
#   US-005 validated the three-layer fix (period reconcile + pool
#   pre-create + memstore↔mon-DB reset). This harness pins the contract
#   so the layered playbook stays exercised under `make smoke-*`.
#
# Skip behaviour: exit 77 when docker / make / aws / jq missing or when
# docker daemon unreachable; REQUIRE_LAB=1 turns the skip into a hard
# fail (CI invocation when the lab IS expected to be present).
#
# Tunables:
#   REQUIRE_LAB              default 0; 1 turns missing-tooling skip into fail
#   SMOKE_RGW_RESTART_CYCLES default 3 (PRD AC)
#   SMOKE_RGW_RESTART_BUCKET default "" → aws s3 ls bare (list buckets)
#   RGW_ENDPOINT_URL         default http://localhost:9991
#   RGW_BENCH_CREDS_FILE     default ./rgw-creds.env (extracted per cycle
#                            from the running rgw container's
#                            /etc/strata-bench/rgw-creds.env)

set -euo pipefail

CYCLES="${SMOKE_RGW_RESTART_CYCLES:-3}"
REQUIRE_LAB="${REQUIRE_LAB:-0}"
RGW_ENDPOINT_URL="${RGW_ENDPOINT_URL:-http://localhost:9991}"
RGW_BENCH_CREDS_FILE="${RGW_BENCH_CREDS_FILE:-./rgw-creds.env}"
COMPOSE="${COMPOSE_CMD:-docker compose -f deploy/docker/docker-compose.yml}"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

skip() {
  local msg="$*"
  if [[ "$REQUIRE_LAB" == "1" ]]; then
    fail "$msg (REQUIRE_LAB=1)"
  fi
  echo "SKIP: $msg" >&2
  exit 77
}

log() {
  printf '[%s] [iter %s] %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "${ITER:-0}" "$*"
}

for tool in docker make aws jq curl; do
  command -v "$tool" >/dev/null 2>&1 \
    || skip "missing tool: $tool"
done

if ! docker info >/dev/null 2>&1; then
  skip "docker daemon unreachable"
fi

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

cleanup() {
  local rc=$?
  if [[ $rc -ne 0 ]]; then
    echo "smoke-rgw-lab-restart: tearing down after failure (rc=$rc)" >&2
    $COMPOSE --profile cassandra --profile tracing --profile webhook-trap \
      --profile ci --profile bench-3replica --profile bench-rgw down \
      >/dev/null 2>&1 || true
  fi
  rm -f "$RGW_BENCH_CREDS_FILE" 2>/dev/null || true
}
trap cleanup EXIT

extract_rgw_creds() {
  $COMPOSE exec -T rgw cat /etc/strata-bench/rgw-creds.env \
    > "$RGW_BENCH_CREDS_FILE" \
    || fail "could not exec rgw container to extract bench creds"
  RGW_AK="$(grep -E '^access_key=' "$RGW_BENCH_CREDS_FILE" | head -1 | cut -d= -f2-)"
  RGW_SK="$(grep -E '^secret_key=' "$RGW_BENCH_CREDS_FILE" | head -1 | cut -d= -f2-)"
  [[ -n "$RGW_AK" && -n "$RGW_SK" ]] \
    || fail "could not parse access_key/secret_key from $RGW_BENCH_CREDS_FILE"
}

start_time="$(date +%s)"

for (( ITER=1; ITER<=CYCLES; ITER++ )); do
  log "rgw-up: make up-all"
  make up-all >/dev/null 2>&1 \
    || fail "make up-all failed on iter $ITER"

  log "rgw-up: wait-tikv + wait-ceph"
  make wait-tikv >/dev/null 2>&1 \
    || fail "make wait-tikv failed on iter $ITER"
  make wait-ceph >/dev/null 2>&1 \
    || fail "make wait-ceph failed on iter $ITER"

  log "rgw-up: make up-bench-rgw"
  make up-bench-rgw >/dev/null 2>&1 \
    || fail "make up-bench-rgw failed on iter $ITER"

  log "rgw-wait-ok: make wait-rgw"
  make wait-rgw >/dev/null 2>&1 \
    || fail "make wait-rgw failed on iter $ITER"

  log "rgw-wait-ok: extract bench creds"
  extract_rgw_creds

  log "rgw-wait-ok: aws s3 ls (list buckets)"
  AWS_ACCESS_KEY_ID="$RGW_AK" AWS_SECRET_ACCESS_KEY="$RGW_SK" \
    aws --endpoint-url "$RGW_ENDPOINT_URL" --no-cli-pager s3 ls >/dev/null 2>&1 \
    || fail "aws s3 ls against rgw failed on iter $ITER"

  log "rgw-down: make down"
  make down >/dev/null 2>&1 \
    || fail "make down failed on iter $ITER"
done

dur=$(( $(date +%s) - start_time ))
echo "PASS: $CYCLES restart cycles completed in ${dur}s"
trap - EXIT
