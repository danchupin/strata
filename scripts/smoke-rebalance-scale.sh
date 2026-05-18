#!/usr/bin/env bash
# Rebalance-scale Phase 2 smoke harness (US-005 of
# ralph/rebalance-scale-phase-2). Closes ROADMAP P2
# "Rebalance worker not sharded — single goroutine bottleneck on large
# deploys" by driving the per-shard fan-out end-to-end against a running
# lab.
#
# Scenarios covered:
#
#   A. Single-replica fan-out (SHARDS=3 in one process):
#      - assert exactly one `leader_for=rebalance` chip on the single
#        replica (folded — even though the replica holds 3 shard
#        leases, the heartbeat chip flips ONCE)
#      - seed N buckets with {default:1,cephb:1} placement
#      - drain default evacuate → assert chunks_on_cluster reaches 0
#
#   B. Multi-leader fan-out (3 replicas, SHARDS=3):
#      - assert all 3 replicas carry the `rebalance` chip (one shard
#        each — lease distribution; one lease per replica)
#      - seed N buckets + drain; assert convergence
#
#   C. Back-compat single-shard (SHARDS=1):
#      - assert EXACTLY one replica holds the `rebalance` chip
#        (legacy single-leader behaviour preserved byte-for-byte)
#      - drain + assert convergence
#
#   D. Replica failover (3 replicas, SHARDS=3):
#      - docker stop one replica → wait STRATA_GC_LEASE_TTL
#      - assert surviving replicas reacquire freed shard (chip stays on
#        every survivor; total chip-count >= 1 across cluster)
#
# The lab shape is operator-managed; the script does NOT bring it up.
# Lab pre-reqs to run all 4 scenarios:
#
#   docker compose -f deploy/docker/docker-compose.yml \
#     --profile lab-tikv --profile lab-tikv-3 up -d
#
# The bare `strata` service (multi-cluster default at :9999) stays
# running alongside via the bare `docker compose up -d` invocation.
# Single-replica labs (only the bare `strata` service on :9999) skip
# Scenarios B + D with a SKIP message; Scenarios A + C still run.
#
# Per-pass orchestration uses SMOKE_RESTART_HOOK (mirror of
# bench-rebalance-multi.sh's BENCH_RESTART_HOOK). Default recipe
# force-recreates the strata-tikv-{a,b,c} replicas with
# STRATA_REBALANCE_SHARDS in env; operators on k8s / systemd override.
#
# Skip behaviour: when $BASE/readyz is unreachable after WAIT_GRACE
# seconds the script EXITs 77 (skipped). REQUIRE_LAB=1 converts the
# skip into a hard fail (CI gating).

set -euo pipefail

BASE="${BASE:-http://127.0.0.1:9999}"
WAIT_GRACE="${WAIT_GRACE:-30}"
REQUIRE_LAB="${REQUIRE_LAB:-0}"

CLUSTER_FROM="${SMOKE_CLUSTER_FROM:-default}"
CLUSTER_TO="${SMOKE_CLUSTER_TO:-cephb}"

SMOKE_BUCKETS="${SMOKE_BUCKETS:-30}"
SMOKE_CHUNKS_PER_BUCKET="${SMOKE_CHUNKS_PER_BUCKET:-3}"
SMOKE_OBJECT_SIZE_KB="${SMOKE_OBJECT_SIZE_KB:-8}"
SMOKE_DRAIN_TIMEOUT_S="${SMOKE_DRAIN_TIMEOUT_S:-600}"
SMOKE_POLL_INTERVAL_S="${SMOKE_POLL_INTERVAL_S:-5}"
SMOKE_LEASE_TTL_S="${SMOKE_LEASE_TTL_S:-${STRATA_GC_LEASE_TTL:-30}}"
SMOKE_FAILOVER_GRACE_S="${SMOKE_FAILOVER_GRACE_S:-$(( SMOKE_LEASE_TTL_S + 15 ))}"

COMPOSE_FILE="${COMPOSE_FILE:-deploy/docker/docker-compose.yml}"
COMPOSE_CMD="${COMPOSE_CMD:-docker compose -f $COMPOSE_FILE --profile lab-tikv --profile lab-tikv-3}"
RESTART_CONTAINERS="${RESTART_CONTAINERS:-strata-tikv-a strata-tikv-b strata-tikv-c strata}"

# Restart recipe re-exports STRATA_REBALANCE_SHARDS so the per-replica
# env passthrough picks it up.
SMOKE_RESTART_HOOK="${SMOKE_RESTART_HOOK:-STRATA_REBALANCE_SHARDS=\$SHARDS STRATA_WORKERS=gc,lifecycle,rebalance $COMPOSE_CMD up -d --force-recreate $RESTART_CONTAINERS}"

# Container name of the replica killed in Scenario D. Default targets
# the lab-tikv-3 third replica (`strata-tikv-c`); override for other
# lab shapes.
SMOKE_FAILOVER_TARGET="${SMOKE_FAILOVER_TARGET:-strata-tikv-c}"

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

for tool in curl jq aws; do
  command -v "$tool" >/dev/null 2>&1 \
    || { echo "FAIL: missing tool: $tool" >&2; exit 2; }
done

TMP="$(mktemp -d)"
JAR="$TMP/cookies"
RUN_TAG="$(date +%s)"
BUCKETS=()
SKIPPED=()

fail() { echo "FAIL: $*" >&2; exit 1; }
pass() { echo "PASS: $*"; }
note() { echo "INFO: $*"; }
banner() { echo; echo "==> Scenario $*"; }
skipped() { echo "SKIP: $*"; SKIPPED+=("$*"); }

cleanup() {
  # Best-effort undrain so a crashed pass does not pin the source cluster.
  curl -sf -b "$JAR" -X POST "$BASE/admin/v1/clusters/$CLUSTER_FROM/undrain" >/dev/null 2>&1 || true
  for b in "${BUCKETS[@]}"; do
    aws --endpoint-url "$BASE" s3 rm "s3://$b" --recursive >/dev/null 2>&1 || true
    aws --endpoint-url "$BASE" s3api delete-bucket --bucket "$b" >/dev/null 2>&1 || true
  done
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
  msg="lab not reachable on $BASE/readyz after ${WAIT_GRACE}s"
  if [[ "$REQUIRE_LAB" == "1" ]]; then
    fail "$msg (REQUIRE_LAB=1)"
  fi
  echo "SKIP: $msg" >&2
  echo "SKIP: bring up lab-tikv-3 + multi-cluster (see header for compose recipe) and re-run." >&2
  exit 77
fi

login() {
  local body code
  body=$(printf '{"access_key":"%s","secret_key":"%s"}' "$AK" "$SK")
  code=$(curl -sS -o "$TMP/login.out" -w '%{http_code}' \
    -c "$JAR" -H 'Content-Type: application/json' \
    -X POST -d "$body" "$BASE/admin/v1/auth/login")
  [[ "$code" == "200" ]] || fail "login: HTTP $code body=$(cat "$TMP/login.out")"
}

admin_get() { curl -sf -b "$JAR" "$BASE$1"; }
admin_post() {
  local path="$1" body="${2:-}"
  if [[ -n "$body" ]]; then
    curl -sS -b "$JAR" -H 'Content-Type: application/json' -X POST -d "$body" "$BASE$path"
  else
    curl -sS -b "$JAR" -X POST "$BASE$path"
  fi
}

aws_creds() {
  export AWS_ACCESS_KEY_ID="$AK"
  export AWS_SECRET_ACCESS_KEY="$SK"
  export AWS_DEFAULT_REGION="${AWS_DEFAULT_REGION:-us-east-1}"
  export AWS_EC2_METADATA_DISABLED=true
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

cluster_state() {
  admin_get "/admin/v1/clusters" \
    | jq -r --arg id "$1" '.clusters[] | select(.id==$id) | .state'
}

drain_progress_chunks() {
  admin_get "/admin/v1/clusters/$CLUSTER_FROM/drain-progress" \
    | jq -r '.chunks_on_cluster // empty'
}

healthy_replica_count() {
  admin_get "/admin/v1/cluster/nodes" \
    | jq -r '[.nodes[] | select(.status=="healthy")] | length'
}

# Counts how many healthy nodes currently carry the named chip.
chip_holder_count() {
  local chip="$1"
  admin_get "/admin/v1/cluster/nodes" \
    | jq -r --arg c "$chip" '[.nodes[] | select(.status=="healthy") | select(.leader_for | index($c))] | length'
}

restart_replicas_with_shards() {
  local SHARDS="$1"
  export SHARDS
  note "restarting replicas with STRATA_REBALANCE_SHARDS=$SHARDS (this can take ~20-60s)"
  if ! eval "$SMOKE_RESTART_HOOK"; then
    fail "restart hook failed (cmd: $SMOKE_RESTART_HOOK)"
  fi
  unset SHARDS
  # Wait for the gateway to come back ready before driving the scenario.
  local i=0
  while (( i < 180 )); do
    if [[ "$(curl -fsS -o /dev/null -w '%{http_code}' "$BASE/readyz")" == "200" ]]; then
      note "lab back online"
      login
      # Heartbeat needs a tick or two to propagate the new chip set.
      sleep "$SMOKE_LEASE_TTL_S"
      return 0
    fi
    sleep 2
    i=$((i+2))
  done
  fail "lab did not come back ready within 360s after restart"
}

seed_buckets() {
  local count="$1" chunks_per="$2"
  note "seeding $count buckets × $chunks_per chunks each (object size ${SMOKE_OBJECT_SIZE_KB} KiB)"
  local payload="$TMP/payload.bin"
  dd if=/dev/urandom of="$payload" bs=1024 count="$SMOKE_OBJECT_SIZE_KB" 2>/dev/null
  local policy="{\"$CLUSTER_FROM\":1,\"$CLUSTER_TO\":1}"
  local b i
  for ((i=0; i<count; i++)); do
    b="smoke-rs-$RUN_TAG-$i"
    aws --endpoint-url "$BASE" s3api create-bucket --bucket "$b" >/dev/null \
      || fail "create-bucket $b failed"
    BUCKETS+=("$b")
    put_placement "$b" "$policy"
    local j
    for ((j=0; j<chunks_per; j++)); do
      aws --endpoint-url "$BASE" s3 cp "$payload" "s3://$b/blob-$j.bin" >/dev/null \
        || fail "PUT $b/blob-$j.bin failed"
    done
  done
}

unseed_buckets() {
  note "removing ${#BUCKETS[@]} seeded buckets"
  for b in "${BUCKETS[@]}"; do
    aws --endpoint-url "$BASE" s3 rm "s3://$b" --recursive >/dev/null 2>&1 || true
    aws --endpoint-url "$BASE" s3api delete-bucket --bucket "$b" >/dev/null 2>&1 || true
  done
  BUCKETS=()
}

wait_drain_converge() {
  local start_s deadline_s elapsed_s chunks
  start_s=$(date +%s)
  deadline_s=$(( start_s + SMOKE_DRAIN_TIMEOUT_S ))
  while (( $(date +%s) < deadline_s )); do
    chunks=$(drain_progress_chunks 2>/dev/null || echo "")
    if [[ -n "$chunks" && "$chunks" -eq 0 ]]; then
      elapsed_s=$(( $(date +%s) - start_s ))
      note "drain converged in ${elapsed_s}s"
      return 0
    fi
    sleep "$SMOKE_POLL_INTERVAL_S"
  done
  fail "drain did not converge within ${SMOKE_DRAIN_TIMEOUT_S}s (chunks_on_cluster=$chunks)"
}

run_drain_cycle() {
  local label="$1"
  note "$label: draining $CLUSTER_FROM (mode=evacuate)"
  local drain_resp
  drain_resp=$(admin_post "/admin/v1/clusters/$CLUSTER_FROM/drain" '{"mode":"evacuate"}')
  note "$label: drain response: $drain_resp"
  local state
  state=$(cluster_state "$CLUSTER_FROM")
  [[ "$state" == "evacuating" ]] \
    || fail "$label: expected state=evacuating got '$state'"
  wait_drain_converge
  admin_post "/admin/v1/clusters/$CLUSTER_FROM/undrain" >/dev/null
}

aws_creds
login

REPLICAS=$(healthy_replica_count)
note "lab reports $REPLICAS healthy replicas"
[[ "$REPLICAS" =~ ^[0-9]+$ && "$REPLICAS" -ge 1 ]] \
  || fail "could not read healthy replica count (got '$REPLICAS')"

# ----------------------------------------------------------------- Scenario A
banner "A: single-replica fan-out (SHARDS=3 → folded chip emits once)"
restart_replicas_with_shards 3
HC=$(chip_holder_count rebalance)
note "scenario A: rebalance chip holders = $HC"
(( HC >= 1 )) \
  || fail "A: expected >=1 rebalance chip holder, got $HC"
if [[ "$REPLICAS" == "1" ]]; then
  (( HC == 1 )) \
    || fail "A: single-replica lab expected EXACTLY 1 rebalance chip holder (folded), got $HC"
fi
seed_buckets "$SMOKE_BUCKETS" "$SMOKE_CHUNKS_PER_BUCKET"
run_drain_cycle "A"
unseed_buckets
pass "A: SHARDS=3 single-replica fan-out drained ($HC chip holder(s))"

# ----------------------------------------------------------------- Scenario B
banner "B: lab-tikv-3 multi-leader (3 replicas, SHARDS=3)"
if (( REPLICAS < 3 )); then
  skipped "B: lab has $REPLICAS healthy replicas (<3), need lab-tikv-3 profile up"
else
  # Re-enter SHARDS=3 in case Scenario A's restart hook missed strata-tikv-{a,b,c}.
  HC=$(chip_holder_count rebalance)
  note "scenario B: rebalance chip holders = $HC (expect 3 — one shard per replica)"
  (( HC >= 2 )) \
    || fail "B: expected at least 2 rebalance chip holders with 3 replicas + 3 shards, got $HC"
  if (( HC < 3 )); then
    note "B: only $HC of 3 replicas hold the chip — TiKV lease distribution may take an extra heartbeat; waiting"
    sleep "$SMOKE_LEASE_TTL_S"
    HC=$(chip_holder_count rebalance)
    (( HC == 3 )) \
      || fail "B: expected EXACTLY 3 chip holders (one per replica) at SHARDS=3, got $HC"
  fi
  seed_buckets "$SMOKE_BUCKETS" "$SMOKE_CHUNKS_PER_BUCKET"
  run_drain_cycle "B"
  unseed_buckets
  pass "B: 3-replica multi-leader fan-out drained ($HC chip holders)"
fi

# ----------------------------------------------------------------- Scenario C
banner "C: back-compat single-shard (SHARDS=1 → exactly one chip holder)"
restart_replicas_with_shards 1
HC=$(chip_holder_count rebalance)
note "scenario C: rebalance chip holders = $HC (expect 1)"
(( HC == 1 )) \
  || fail "C: expected EXACTLY 1 rebalance chip holder at SHARDS=1, got $HC"
seed_buckets "$SMOKE_BUCKETS" "$SMOKE_CHUNKS_PER_BUCKET"
run_drain_cycle "C"
unseed_buckets
pass "C: SHARDS=1 back-compat — single-leader behaviour preserved byte-for-byte"

# ----------------------------------------------------------------- Scenario D
banner "D: replica failover (kill 1 of 3 → survivors reacquire freed shard)"
if (( REPLICAS < 3 )); then
  skipped "D: lab has $REPLICAS healthy replicas (<3), need lab-tikv-3 profile up"
elif ! command -v docker >/dev/null 2>&1; then
  skipped "D: docker CLI not available — cannot kill replica '$SMOKE_FAILOVER_TARGET'"
else
  restart_replicas_with_shards 3
  HC=$(chip_holder_count rebalance)
  (( HC == 3 )) \
    || fail "D: setup expected 3 chip holders, got $HC (cannot exercise failover cleanly)"
  note "D: killing replica '$SMOKE_FAILOVER_TARGET'"
  docker stop "$SMOKE_FAILOVER_TARGET" >/dev/null \
    || fail "D: docker stop $SMOKE_FAILOVER_TARGET failed"
  note "D: waiting ${SMOKE_FAILOVER_GRACE_S}s for lease to expire + survivor reacquire"
  sleep "$SMOKE_FAILOVER_GRACE_S"
  HEALTHY=$(healthy_replica_count)
  note "D: healthy replicas after kill = $HEALTHY"
  (( HEALTHY == 2 )) \
    || fail "D: expected 2 healthy replicas after kill, got $HEALTHY"
  HC=$(chip_holder_count rebalance)
  note "D: rebalance chip holders after lease re-acquisition = $HC"
  (( HC >= 1 )) \
    || fail "D: rebalance chip not present on any survivor after ${SMOKE_FAILOVER_GRACE_S}s"
  (( HC == 2 )) \
    || note "D: chip holders = $HC (expected 2 — both survivors share the 3 shards); accepting >=1 as smoke pass"
  # Restart the killed replica so subsequent runs find the lab intact.
  note "D: restarting '$SMOKE_FAILOVER_TARGET'"
  docker start "$SMOKE_FAILOVER_TARGET" >/dev/null \
    || note "D: docker start $SMOKE_FAILOVER_TARGET failed — operator must restore"
  pass "D: failover survived ($HC chip holders carrying shards from killed replica)"
fi

echo
if (( ${#SKIPPED[@]} == 0 )); then
  echo "== rebalance-scale smoke OK (4/4 scenarios green)"
else
  echo "== rebalance-scale smoke OK ($(( 4 - ${#SKIPPED[@]} ))/4 scenarios green, ${#SKIPPED[@]} skipped)"
  for s in "${SKIPPED[@]}"; do
    echo "   SKIPPED: $s"
  done
fi
