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
PROM_BASE="${PROM_BASE:-http://127.0.0.1:9090}"
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
EXPECTED_REPLICAS="${SMOKE_DPU_EXPECTED_REPLICAS:-2}"
EXPECTED_RATE_MB_S="${SMOKE_DPU_EXPECTED_RATE_MB_S:-1}"
EXPECTED_GRACE_S="${SMOKE_DPU_EXPECTED_GRACE_S:-60}"

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
SAMPLED_PROM_RATE=0
SAMPLED_GC_CONFIG=0

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

gc_config()        { curl -sf -b "$JAR" "$BASE/admin/v1/gc-config"; }
rebalance_config() { curl -sf -b "$JAR" "$BASE/admin/v1/rebalance-config"; }

# Sample sum(rate(strata_rebalance_bytes_moved_total{to="<cluster>"}[1m])) MB/s.
# Returns numeric MB/s on stdout (0 on parse miss / Prom 4xx-5xx).
prom_bytes_rate() {
  local q out
  q="rate(strata_rebalance_bytes_moved_total%7Bto%3D%22${CLUSTER}%22%7D%5B1m%5D)"
  out=$(curl -sf "${PROM_BASE}/api/v1/query?query=${q}" 2>/dev/null) || { echo 0; return; }
  local v
  v=$(echo "$out" | jq -r '
    (.data.result // [])
    | (if length == 0 then 0
       else (map(.value[1] | tonumber) | add) end)' 2>/dev/null) || v=0
  awk -v b="$v" 'BEGIN { printf "%.3f", b / 1048576.0 }'
}

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

banner "GET /admin/v1/rebalance-config — assert rate_mb_s=$EXPECTED_RATE_MB_S + replicas_count>=$EXPECTED_REPLICAS"
RBC=$(rebalance_config) || fail "rebalance-config: HTTP error (gateway must expose endpoint per US-001)"
rbc_rate=$(echo "$RBC" | jq -r '.rate_mb_s // empty')
rbc_replicas=$(echo "$RBC" | jq -r '.replicas_count // empty')
rbc_interval=$(echo "$RBC" | jq -r '.interval_seconds // empty')
rbc_inflight=$(echo "$RBC" | jq -r '.inflight // empty')
rbc_shards=$(echo "$RBC" | jq -r '.shards // empty')
note "rebalance-config: rate_mb_s=$rbc_rate interval_seconds=$rbc_interval inflight=$rbc_inflight shards=$rbc_shards replicas_count=$rbc_replicas"
[[ "$rbc_rate" == "$EXPECTED_RATE_MB_S" ]] \
  || fail "rebalance-config rate_mb_s=$rbc_rate; expected $EXPECTED_RATE_MB_S (smoke env STRATA_REBALANCE_RATE_MB_S=1)"
[[ -n "$rbc_replicas" && "$rbc_replicas" -ge "$EXPECTED_REPLICAS" ]] \
  || fail "rebalance-config replicas_count=$rbc_replicas; expected >= $EXPECTED_REPLICAS (lab default strata-a + strata-b)"
pass "rebalance-config shape valid (rate_mb_s=$rbc_rate replicas_count=$rbc_replicas)"

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

  # Migrating: sample Prom 1m byte-rate ONCE while migration is hot.
  if [[ "$state" == "Migrating" && "$SAMPLED_PROM_RATE" == "0" ]]; then
    mbs=$(prom_bytes_rate)
    note "prom rate(strata_rebalance_bytes_moved_total{to=\"$CLUSTER\"}[1m]) = ${mbs} MB/s"
    # `awk` boolean returns 1 when nonzero.
    if awk -v m="$mbs" 'BEGIN { exit !(m > 0) }'; then
      pass "Prom byte-rate > 0 MB/s (migration actively moving)"
      SAMPLED_PROM_RATE=1
    else
      note "Prom byte-rate ~0 — retry on next poll (1m window may not have warmed yet)"
    fi
  fi

  # AwaitingGC: sample /admin/v1/gc-config ONCE + assert formula vs polled gc_queue_pending.
  if [[ "$state" == "AwaitingGC" && "$SAMPLED_GC_CONFIG" == "0" ]]; then
    GCC=$(gc_config) || fail "gc-config: HTTP error (gateway must expose endpoint per US-001)"
    gcc_grace=$(echo "$GCC" | jq -r '.grace_seconds // empty')
    gcc_interval=$(echo "$GCC" | jq -r '.interval_seconds // empty')
    gcc_batch=$(echo "$GCC" | jq -r '.batch_size // empty')
    gcc_concurrency=$(echo "$GCC" | jq -r '.concurrency // empty')
    gcc_shards=$(echo "$GCC" | jq -r '.shards // empty')
    note "gc-config: grace_seconds=$gcc_grace interval_seconds=$gcc_interval batch_size=$gcc_batch concurrency=$gcc_concurrency shards=$gcc_shards"
    [[ "$gcc_grace" == "$EXPECTED_GRACE_S" ]] \
      || fail "gc-config grace_seconds=$gcc_grace; expected $EXPECTED_GRACE_S (smoke env STRATA_GC_GRACE=60s)"
    # eta_min = ceil(grace/60) + ceil((queue/(batch*shards*1)) * (interval/60))
    eta_min=$(awk -v g="$gcc_grace" -v i="$gcc_interval" -v b="$gcc_batch" -v s="$gcc_shards" -v q="$gcq" '
      function ceil(x) { return (x == int(x)) ? x : int(x) + 1 }
      BEGIN {
        if (b < 1) b = 1
        if (s < 1) s = 1
        if (i < 1) i = 1
        gm = ceil(g / 60.0)
        qm = ceil((q / (b * s)) * (i / 60.0))
        eta = gm + qm
        if (eta > 1440) eta = 1440
        printf "%d", eta
      }')
    note "eta_min formula (grace=$gcc_grace, interval=$gcc_interval, batch=$gcc_batch, shards=$gcc_shards, queue=$gcq) = $eta_min min"
    [[ "$eta_min" -ge 0 && "$eta_min" -le 30 ]] \
      || fail "eta_min=$eta_min out of sane band [0, 30] for smoke env"
    pass "gc-config shape valid + eta_min=$eta_min within [0, 30]"
    SAMPLED_GC_CONFIG=1
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

(( SAMPLED_PROM_RATE == 1 )) \
  || fail "Prom byte-rate query (strata_rebalance_bytes_moved_total{to=\"$CLUSTER\"}[1m]) never reported > 0 MB/s while Migrating — check $PROM_BASE reachability + scrape config"
(( SAMPLED_GC_CONFIG == 1 )) \
  || fail "never sampled /admin/v1/gc-config during AwaitingGC — state may have flipped too fast or endpoint missing"

echo
echo "== drain-progress-ui smoke OK (gc-config + rebalance-config + Prom rate verified)"
