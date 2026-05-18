#!/usr/bin/env bash
# Drain-progress 3-state smoke harness (US-003 of the
# ralph/drain-progress-physical cycle).
#
# Validates the operator-perceived 3-state progression on
# `/admin/v1/clusters/<id>/drain-progress`:
#
#   1. Migrating         — physical_chunks_on_cluster > 0
#                          AND chunks_on_cluster > 0
#   2. Awaiting GC       — physical_chunks_on_cluster > 0
#                          AND chunks_on_cluster == 0
#                          AND gc_queue_pending > 0
#   3. Ready to deregister — physical_chunks_on_cluster == 0
#                          AND chunks_on_cluster == 0
#                          AND gc_queue_pending == 0
#                          AND deregister_ready == true
#
# Why throttled env (smoke-only — NOT a prod default):
#   STRATA_REBALANCE_RATE_MB_S=1 stretches the Migrating phase wide
#     enough to sample with a 3 s poll cadence — at the 100 MB/s prod
#     default the 300-chunk migration completes faster than one poll
#     interval and the Migrating state is invisible from the operator
#     console.
#   STRATA_GC_GRACE=60s shortens the GC grace from 5m (prod default)
#     so the entire smoke completes within the 5-min budget. Without
#     the shortened grace the Awaiting GC phase alone would exceed
#     the script timeout.
#
# Pre-requisites on the host:
#   docker, curl, jq, aws (>= 2)
#   STRATA_STATIC_CREDENTIALS=<access:secret[:owner]>... exported with
#   the same value the gateway booted with (first comma-separated
#   entry used for admin login + SigV4 PUT).
#
# Lab assumptions:
#   - bare `docker compose up -d` stack is the canonical multi-cluster
#     shape: `cassandra + ceph + ceph-b + strata` (port 9999).
#   - STRATA_RADOS_CLUSTERS=default:...,cephb:...
#
# Skip behavior: when the compose stack is NOT up (probe on
# /readyz fails after WAIT_GRACE seconds), the script EXITs 77
# (skipped) unless REQUIRE_LAB=1.

set -euo pipefail

BASE="${BASE:-http://127.0.0.1:9999}"
WAIT_GRACE="${WAIT_GRACE:-30}"
CLUSTER="${SMOKE_DPU_CLUSTER:-cephb}"
OTHER_CLUSTER="${SMOKE_DPU_OTHER:-default}"
OBJECT_COUNT="${SMOKE_DPU_OBJECTS:-300}"
OBJECT_SIZE_KB="${SMOKE_DPU_OBJECT_KB:-1024}"
POLL_INTERVAL_S="${SMOKE_DPU_POLL_S:-3}"
TIMEOUT_S="${SMOKE_DPU_TIMEOUT_S:-300}"
COMPOSE_FILE="${COMPOSE_FILE:-deploy/docker/docker-compose.yml}"
COMPOSE_CMD="${COMPOSE_CMD:-docker compose -f $COMPOSE_FILE}"
STRATA_CONTAINER="${STRATA_CONTAINER:-strata}"
REQUIRE_LAB="${REQUIRE_LAB:-0}"
STAMP="$(date +%s)"

CRED="${STRATA_STATIC_CREDENTIALS:-}"
if [[ -z "$CRED" ]]; then
  echo "FAIL: STRATA_STATIC_CREDENTIALS unset (need access:secret[:owner])" >&2
  exit 2
fi
FIRST="${CRED%%,*}"
AK="${FIRST%%:*}"
REST="${FIRST#*:}"
SK="${REST%%:*}"
if [[ -z "$AK" || -z "$SK" || "$AK" == "$FIRST" ]]; then
  echo "FAIL: cannot parse access/secret from STRATA_STATIC_CREDENTIALS='$FIRST'" >&2
  exit 2
fi

for tool in curl jq aws docker; do
  command -v "$tool" >/dev/null 2>&1 \
    || { echo "FAIL: missing tool: $tool" >&2; exit 2; }
done

TMP="$(mktemp -d)"
JAR="$TMP/cookies"
BUCKET="dpu-$STAMP"
OBSERVED_MIGRATING=0
OBSERVED_AWAITING_GC=0
OBSERVED_READY=0

fail() { echo "FAIL: $*" >&2; exit 1; }
pass() { echo "PASS: $*"; }
note() { echo "INFO: $*"; }
banner() { echo; echo "==> $*"; }

cleanup() {
  banner "Cleanup: undrain $CLUSTER + remove bucket $BUCKET + restore prod env on $STRATA_CONTAINER"
  curl -sf -b "$JAR" -X POST "$BASE/admin/v1/clusters/$CLUSTER/undrain" \
    >/dev/null 2>&1 || true
  aws --endpoint-url "$BASE" s3 rm "s3://$BUCKET" --recursive >/dev/null 2>&1 || true
  aws --endpoint-url "$BASE" s3api delete-bucket --bucket "$BUCKET" >/dev/null 2>&1 || true
  # Restore strata to prod-default env (drop the throttled overrides).
  unset STRATA_REBALANCE_RATE_MB_S STRATA_GC_GRACE
  $COMPOSE_CMD up -d --no-deps "$STRATA_CONTAINER" >/dev/null 2>&1 || true
  rm -rf "$TMP"
}
trap cleanup EXIT

probe_ready() {
  local i=0
  while (( i < WAIT_GRACE )); do
    if [[ "$(curl -fsS -o /dev/null -w '%{http_code}' "$BASE/readyz")" == "200" ]]; then
      return 0
    fi
    sleep 1
    i=$((i+1))
  done
  return 1
}

if ! probe_ready; then
  msg="compose stack not reachable on $BASE/readyz after ${WAIT_GRACE}s"
  if [[ "$REQUIRE_LAB" == "1" ]]; then
    fail "$msg (REQUIRE_LAB=1)"
  fi
  echo "SKIP: $msg" >&2
  echo "SKIP: bring up the bare compose stack via 'docker compose up -d' and re-run." >&2
  exit 77
fi

banner "Reconfigure $STRATA_CONTAINER with throttled smoke env"
note "STRATA_REBALANCE_RATE_MB_S=1 STRATA_GC_GRACE=60s — smoke-only; NOT prod defaults."
STRATA_REBALANCE_RATE_MB_S=1 STRATA_GC_GRACE=60s \
  $COMPOSE_CMD up -d --no-deps --force-recreate "$STRATA_CONTAINER" >/dev/null \
  || fail "compose: failed to recreate $STRATA_CONTAINER with throttled env"

if ! probe_ready; then
  fail "gateway did not become ready after recreate (${WAIT_GRACE}s)"
fi
pass "throttled strata container ready"

login() {
  local body code
  body=$(printf '{"access_key":"%s","secret_key":"%s"}' "$AK" "$SK")
  code=$(curl -sS -o "$TMP/login.out" -w '%{http_code}' \
    -c "$JAR" -H 'Content-Type: application/json' \
    -X POST -d "$body" "$BASE/admin/v1/auth/login")
  [[ "$code" == "200" ]] || fail "login: HTTP $code body=$(cat "$TMP/login.out")"
}

put_placement() {
  local bucket="$1" policy="$2"
  local code
  code=$(curl -sS -o "$TMP/placement.out" -w '%{http_code}' \
    -b "$JAR" -H 'Content-Type: application/json' \
    -X PUT -d "{\"placement\":$policy}" \
    "$BASE/admin/v1/buckets/$bucket/placement")
  [[ "$code" == "200" || "$code" == "204" ]] \
    || fail "PUT placement $bucket: HTTP $code body=$(cat "$TMP/placement.out")"
}

drain_progress() { curl -sf -b "$JAR" "$BASE/admin/v1/clusters/$CLUSTER/drain-progress"; }

export AWS_ACCESS_KEY_ID="$AK"
export AWS_SECRET_ACCESS_KEY="$SK"
export AWS_DEFAULT_REGION="${AWS_DEFAULT_REGION:-us-east-1}"
export AWS_EC2_METADATA_DISABLED=true

login

banner "Seed $OBJECT_COUNT × ~${OBJECT_SIZE_KB}KB objects on $BUCKET (Placement={$OTHER_CLUSTER:1,$CLUSTER:1})"
aws --endpoint-url "$BASE" s3api create-bucket --bucket "$BUCKET" >/dev/null \
  || fail "create-bucket $BUCKET failed"
put_placement "$BUCKET" "{\"$OTHER_CLUSTER\":1,\"$CLUSTER\":1}"

PAYLOAD="$TMP/payload.bin"
dd if=/dev/urandom of="$PAYLOAD" bs=1024 count="$OBJECT_SIZE_KB" 2>/dev/null

for ((i=1; i<=OBJECT_COUNT; i++)); do
  aws --endpoint-url "$BASE" s3 cp "$PAYLOAD" "s3://$BUCKET/o-$i.bin" >/dev/null \
    || fail "PUT o-$i.bin failed (seeded $((i-1)) of $OBJECT_COUNT)"
  if (( i % 50 == 0 )); then note "seeded $i / $OBJECT_COUNT"; fi
done
pass "seeded $OBJECT_COUNT objects"

banner "Drain $CLUSTER mode=evacuate"
CODE=$(curl -sS -o "$TMP/drain.out" -w '%{http_code}' \
  -b "$JAR" -H 'Content-Type: application/json' \
  -X POST -d '{"mode":"evacuate"}' \
  "$BASE/admin/v1/clusters/$CLUSTER/drain")
[[ "$CODE" == "200" || "$CODE" == "204" ]] \
  || fail "drain: HTTP $CODE body=$(cat "$TMP/drain.out")"
pass "drain accepted; state=evacuating"

banner "Poll /drain-progress every ${POLL_INTERVAL_S}s (timeout ${TIMEOUT_S}s) — assert all 3 states observed"

deadline=$(( $(date +%s) + TIMEOUT_S ))
last_state="(none)"
while (( $(date +%s) < deadline )); do
  body=$(drain_progress) || { sleep "$POLL_INTERVAL_S"; continue; }
  phys=$(echo "$body" | jq -r '.physical_chunks_on_cluster // empty')
  manifest=$(echo "$body" | jq -r '.chunks_on_cluster // 0')
  gcq=$(echo "$body" | jq -r '.gc_queue_pending // 0')
  ready=$(echo "$body" | jq -r '.deregister_ready // false')

  if [[ -z "$phys" || "$phys" == "null" ]]; then
    fail "physical_chunks_on_cluster is null on $CLUSTER — backend probe must be wired (RADOS expected). body=$body"
  fi

  state="unknown"
  if (( phys > 0 && manifest > 0 )); then
    state="Migrating"
    OBSERVED_MIGRATING=1
  elif (( phys > 0 && manifest == 0 && gcq > 0 )); then
    state="AwaitingGC"
    OBSERVED_AWAITING_GC=1
  elif (( phys == 0 && manifest == 0 && gcq == 0 )) && [[ "$ready" == "true" ]]; then
    state="Ready"
    OBSERVED_READY=1
  fi

  if [[ "$state" != "$last_state" ]]; then
    note "state=$state phys=$phys manifest=$manifest gc_queue=$gcq ready=$ready"
    last_state="$state"
  fi

  if (( OBSERVED_READY == 1 )); then break; fi
  sleep "$POLL_INTERVAL_S"
done

(( OBSERVED_MIGRATING == 1 )) \
  || fail "never observed Migrating state (phys>0 && manifest>0). Final body=$(drain_progress)"
(( OBSERVED_AWAITING_GC == 1 )) \
  || fail "never observed AwaitingGC state (phys>0 && manifest==0 && gc_queue>0). Final body=$(drain_progress)"
(( OBSERVED_READY == 1 )) \
  || fail "never observed Ready state (phys==0 && manifest==0 && gc_queue==0 && deregister_ready=true). Final body=$(drain_progress)"

pass "all three drain-progress states observed (Migrating + AwaitingGC + Ready)"

echo
echo "== drain-progress-ui smoke OK"
