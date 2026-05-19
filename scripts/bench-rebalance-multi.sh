#!/usr/bin/env bash
# Rebalance worker multi-leader bench (US-004 of ralph/rebalance-scale-phase-2).
#
# Measures wall-clock-to-drain for a TiKV-backed lab with the rebalance worker
# fan-out (STRATA_REBALANCE_SHARDS) flipped between 1 (baseline = Phase 1
# behaviour) and 2 (2-replica multi-leader, the new bare-default lab shape).
# Prints both timings, the ratio, and a verdict — `SPEEDUP_OK` if SHARDS=2
# ≤ 40 % of SHARDS=1 (>=2.5x), or `SPEEDUP_FAILED` if SHARDS=2 > 70 % of
# SHARDS=1 (regression guard => exit 1).
#
# SHARDS=3 leg (US-002 of ralph/dx-lab): opt-in. Bring up the third replica
# via `docker compose --profile bench-3replica up -d strata-c` BEFORE
# running this script. The script auto-detects strata-c via
# `docker compose ps -q strata-c`; when present the third pass runs and
# its elapsed_s lands in the results JSONL alongside SHARDS=1/2. When
# absent the leg SKIPs with a clear message and the script still exits
# on the SHARDS=1/2 verdict alone. Tear-down: `make down` (cleans up the
# bench-3replica profile too).
#
# The script does NOT bring up the lab. Operator stands the stack up with:
#
#   STRATA_REBALANCE_SHARDS=1 STRATA_WORKERS=gc,lifecycle,rebalance \
#     docker compose -f deploy/docker/docker-compose.yml up -d
#
# (bare-default = TiKV multi-cluster strata-a + strata-b behind nginx LB on
# :9999.) The script then orchestrates two passes via `BENCH_RESTART_HOOK`
# (default: docker compose force-recreate of the strata-a/b replicas) with
# STRATA_REBALANCE_SHARDS exported between runs. Override BENCH_RESTART_HOOK
# if your lab uses a different restart recipe (k8s rollout, bare-metal
# systemctl, etc.).
#
# Pre-reqs on the host: docker, curl, jq, aws (>=2). The lab gateway must be
# reachable on $BASE (default http://127.0.0.1:9999 — nginx LB port for the
# bare-default lab). STRATA_STATIC_CREDENTIALS exported with the same value
# the gateway booted with (first comma-separated entry is used for admin
# login + SigV4 puts).
#
# Skip behaviour: when the lab is not reachable after WAIT_GRACE seconds the
# script EXITs 77 with a clear message. Set REQUIRE_LAB=1 to convert the skip
# into a hard fail (CI gating).

set -euo pipefail

BASE="${BASE:-http://127.0.0.1:9999}"
WAIT_GRACE="${WAIT_GRACE:-30}"
REQUIRE_LAB="${REQUIRE_LAB:-0}"

CLUSTER_FROM="${BENCH_CLUSTER_FROM:-default}"
CLUSTER_TO="${BENCH_CLUSTER_TO:-cephb}"

BENCH_BUCKETS="${BENCH_BUCKETS:-1000}"
BENCH_CHUNKS_PER_BUCKET="${BENCH_CHUNKS_PER_BUCKET:-10}"
BENCH_OBJECT_SIZE_KB="${BENCH_OBJECT_SIZE_KB:-32}"
BENCH_DRAIN_TIMEOUT_S="${BENCH_DRAIN_TIMEOUT_S:-1800}"
BENCH_POLL_INTERVAL_S="${BENCH_POLL_INTERVAL_S:-5}"
BENCH_SHARDS_BASELINE="${BENCH_SHARDS_BASELINE:-1}"
BENCH_SHARDS_FANOUT="${BENCH_SHARDS_FANOUT:-2}"
BENCH_SHARDS_3REPLICA="${BENCH_SHARDS_3REPLICA:-3}"
BENCH_SPEEDUP_TARGET_PCT="${BENCH_SPEEDUP_TARGET_PCT:-40}"
BENCH_REGRESSION_PCT="${BENCH_REGRESSION_PCT:-70}"
BENCH_RESULTS_FILE="${BENCH_RESULTS_FILE:-bench-rebalance-multi-results.jsonl}"

COMPOSE_FILE="${COMPOSE_FILE:-deploy/docker/docker-compose.yml}"
COMPOSE_CMD="${COMPOSE_CMD:-docker compose -f $COMPOSE_FILE}"
COMPOSE_CMD_3REPLICA="${COMPOSE_CMD_3REPLICA:-docker compose -f $COMPOSE_FILE --profile bench-3replica}"
RESTART_CONTAINERS="${RESTART_CONTAINERS:-strata-a strata-b}"
RESTART_CONTAINERS_3REPLICA="${RESTART_CONTAINERS_3REPLICA:-strata-a strata-b strata-c}"

# BENCH_RESTART_HOOK is the bash command that restarts the strata replicas
# with the SHARDS env baked into the container env. Default is a force-
# recreate via docker compose; the wrapper exports STRATA_REBALANCE_SHARDS
# so the replica env passthrough picks it up. Operators on k8s / systemd
# can override this with a hook that does whatever their lab needs.
BENCH_RESTART_HOOK="${BENCH_RESTART_HOOK:-STRATA_REBALANCE_SHARDS=\$SHARDS STRATA_WORKERS=gc,lifecycle,rebalance $COMPOSE_CMD up -d --force-recreate $RESTART_CONTAINERS}"

# Separate hook for the 3-replica leg — must use the bench-3replica profile
# so strata-c is included in the up/force-recreate set. Override if the
# operator's lab differs (k8s rollout, etc.). The hook is only invoked when
# the 3-replica leg is gated on (strata-c container detected).
BENCH_RESTART_HOOK_3REPLICA="${BENCH_RESTART_HOOK_3REPLICA:-STRATA_REBALANCE_SHARDS=\$SHARDS STRATA_WORKERS=gc,lifecycle,rebalance $COMPOSE_CMD_3REPLICA up -d --force-recreate $RESTART_CONTAINERS_3REPLICA}"

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
  command -v "$tool" >/dev/null 2>&1 || { echo "FAIL: missing tool: $tool" >&2; exit 2; }
done

TMP="$(mktemp -d)"
JAR="$TMP/cookies"
RUN_TAG="$(date +%s)"
BUCKETS=()

fail() { echo "FAIL: $*" >&2; exit 1; }
pass() { echo "PASS: $*"; }
note() { echo "INFO: $*"; }

cleanup() {
  # Best-effort undrain so a crashed run doesn't pin the source cluster.
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
  echo "SKIP: bring up bare-default TiKV lab via 'docker compose up -d' (see header for recipe) and re-run." >&2
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

# validate_lab confirms the gateway exposes both source + target clusters and
# the rebalance worker is wired (drain-progress returns 200 / 503-not-found
# both surface here so misconfigs fail fast).
validate_lab() {
  local clusters_json from_state to_state
  clusters_json=$(admin_get "/admin/v1/clusters") || fail "GET /admin/v1/clusters failed"
  from_state=$(echo "$clusters_json" | jq -r --arg id "$CLUSTER_FROM" '.clusters[] | select(.id==$id) | .state // empty')
  to_state=$(echo "$clusters_json" | jq -r --arg id "$CLUSTER_TO" '.clusters[] | select(.id==$id) | .state // empty')
  [[ -n "$from_state" ]] || fail "source cluster '$CLUSTER_FROM' missing from /admin/v1/clusters (check STRATA_RADOS_CLUSTERS)"
  [[ -n "$to_state" ]]   || fail "target cluster '$CLUSTER_TO' missing from /admin/v1/clusters (check STRATA_RADOS_CLUSTERS)"
  note "lab clusters: $CLUSTER_FROM=$from_state $CLUSTER_TO=$to_state"

  # Probe drain-progress — 503 ProgressUnavailable means rebalance worker absent.
  local code
  code=$(curl -sS -o "$TMP/progress.out" -w '%{http_code}' \
    -b "$JAR" "$BASE/admin/v1/clusters/$CLUSTER_FROM/drain-progress")
  if [[ "$code" == "503" ]] && grep -q ProgressUnavailable "$TMP/progress.out"; then
    fail "rebalance worker not running — start with STRATA_WORKERS=...,rebalance"
  fi
}

restart_replicas_with_shards() {
  local SHARDS="$1"
  local hook="${2:-$BENCH_RESTART_HOOK}"
  export SHARDS
  note "restarting replicas with STRATA_REBALANCE_SHARDS=$SHARDS"
  # eval so the operator-provided hook can interpolate $SHARDS at run time.
  if ! eval "$hook"; then
    fail "restart hook failed (cmd: $hook)"
  fi
  unset SHARDS
  # Wait for the LB to come back ready before driving the bench.
  local i=0
  while (( i < 120 )); do
    if [[ "$(curl -fsS -o /dev/null -w '%{http_code}' "$BASE/readyz")" == "200" ]]; then
      note "lab back online"
      # Re-login since cookie jar may have been invalidated when the gateway
      # process restarted.
      login
      return 0
    fi
    sleep 2
    i=$((i+2))
  done
  fail "lab did not come back ready within 240s after restart"
}

seed_buckets() {
  local count="$1" chunks_per="$2"
  note "seeding $count buckets × $chunks_per chunks each (object size ${BENCH_OBJECT_SIZE_KB} KiB)"
  local payload="$TMP/payload.bin"
  dd if=/dev/urandom of="$payload" bs=1024 count="$BENCH_OBJECT_SIZE_KB" 2>/dev/null
  local policy="{\"$CLUSTER_FROM\":1,\"$CLUSTER_TO\":1}"
  local b i
  for ((i=0; i<count; i++)); do
    b="bench-rmulti-$RUN_TAG-$i"
    aws --endpoint-url "$BASE" s3api create-bucket --bucket "$b" >/dev/null \
      || fail "create-bucket $b failed"
    BUCKETS+=("$b")
    put_placement "$b" "$policy"
    local j
    for ((j=0; j<chunks_per; j++)); do
      aws --endpoint-url "$BASE" s3 cp "$payload" "s3://$b/blob-$j.bin" >/dev/null \
        || fail "PUT $b/blob-$j.bin failed"
    done
    if (( (i+1) % 50 == 0 )); then
      note "  seeded $((i+1))/$count buckets"
    fi
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

run_drain_pass() {
  local shards="$1"
  local hook="${2:-$BENCH_RESTART_HOOK}"
  echo
  echo "== Pass shards=$shards"

  restart_replicas_with_shards "$shards" "$hook"
  validate_lab
  seed_buckets "$BENCH_BUCKETS" "$BENCH_CHUNKS_PER_BUCKET"

  note "drain $CLUSTER_FROM (mode=evacuate)"
  local drain_resp
  drain_resp=$(admin_post "/admin/v1/clusters/$CLUSTER_FROM/drain" '{"mode":"evacuate"}')
  note "drain response: $drain_resp"

  local state
  state=$(cluster_state "$CLUSTER_FROM")
  [[ "$state" == "evacuating" ]] || fail "expected state=evacuating got '$state'"

  local start_s deadline_s elapsed_s chunks
  start_s=$(date +%s)
  deadline_s=$(( start_s + BENCH_DRAIN_TIMEOUT_S ))

  while (( $(date +%s) < deadline_s )); do
    chunks=$(drain_progress_chunks 2>/dev/null || echo "")
    if [[ -n "$chunks" && "$chunks" -eq 0 ]]; then
      elapsed_s=$(( $(date +%s) - start_s ))
      note "drain complete after ${elapsed_s}s"
      # Undrain + unseed so the next pass starts from a clean slate.
      admin_post "/admin/v1/clusters/$CLUSTER_FROM/undrain" >/dev/null
      unseed_buckets
      printf '{"shards":%d,"buckets":%d,"chunks_per_bucket":%d,"object_kb":%d,"elapsed_s":%d}\n' \
        "$shards" "$BENCH_BUCKETS" "$BENCH_CHUNKS_PER_BUCKET" "$BENCH_OBJECT_SIZE_KB" "$elapsed_s" \
        | tee -a "$BENCH_RESULTS_FILE"
      echo "$elapsed_s"
      return 0
    fi
    sleep "$BENCH_POLL_INTERVAL_S"
  done
  fail "drain did not complete within ${BENCH_DRAIN_TIMEOUT_S}s (chunks_on_cluster=$chunks)"
}

aws_creds
login
validate_lab

rm -f "$BENCH_RESULTS_FILE"
note "results JSONL: $BENCH_RESULTS_FILE"

# Capture each pass's wall-clock from the printed JSONL line.
T1=$(run_drain_pass "$BENCH_SHARDS_BASELINE" | tail -n 1)
T2=$(run_drain_pass "$BENCH_SHARDS_FANOUT"   | tail -n 1)

if ! [[ "$T1" =~ ^[0-9]+$ ]]; then fail "could not parse baseline elapsed: '$T1'"; fi
if ! [[ "$T2" =~ ^[0-9]+$ ]]; then fail "could not parse fanout   elapsed: '$T2'"; fi

# Ratio = T2 / T1, expressed as integer percent (rounded).
RATIO_PCT=$(( T2 * 100 / (T1 == 0 ? 1 : T1) ))

echo
echo "== Summary"
printf "  shards=%d  elapsed=%ds\n" "$BENCH_SHARDS_BASELINE" "$T1"
printf "  shards=%d  elapsed=%ds\n" "$BENCH_SHARDS_FANOUT"   "$T2"
printf "  ratio    = %d %% (target ≤%d %%, regression >%d %%)\n" \
  "$RATIO_PCT" "$BENCH_SPEEDUP_TARGET_PCT" "$BENCH_REGRESSION_PCT"

# SHARDS=3 leg (US-002 of ralph/dx-lab). Auto-detect strata-c via
# `docker compose ps -q strata-c`; opt-in via `docker compose
# --profile bench-3replica up -d strata-c` before running.
T3=""
if [[ -n "$(eval "$COMPOSE_CMD_3REPLICA ps -q strata-c" 2>/dev/null)" ]]; then
  note "strata-c detected; running SHARDS=$BENCH_SHARDS_3REPLICA leg"
  T3=$(run_drain_pass "$BENCH_SHARDS_3REPLICA" "$BENCH_RESTART_HOOK_3REPLICA" | tail -n 1)
  if ! [[ "$T3" =~ ^[0-9]+$ ]]; then fail "could not parse 3-replica elapsed: '$T3'"; fi
  RATIO_PCT_3=$(( T3 * 100 / (T1 == 0 ? 1 : T1) ))
  printf "  shards=%d  elapsed=%ds  ratio_vs_baseline=%d %%\n" \
    "$BENCH_SHARDS_3REPLICA" "$T3" "$RATIO_PCT_3"
else
  echo "SKIP: SHARDS=$BENCH_SHARDS_3REPLICA leg requires the bench-3replica profile (3 strata replicas + 2 ceph clusters + pd + tikv may exceed lima/macOS memory caps; bring up via 'docker compose --profile bench-3replica up -d strata-c' when host has headroom)"
fi

if (( RATIO_PCT <= BENCH_SPEEDUP_TARGET_PCT )); then
  echo "VERDICT: SPEEDUP_OK"
  exit 0
elif (( RATIO_PCT > BENCH_REGRESSION_PCT )); then
  echo "VERDICT: SPEEDUP_FAILED (regression: shards=$BENCH_SHARDS_FANOUT slower than expected)"
  exit 1
else
  echo "VERDICT: SPEEDUP_PARTIAL (between target and regression threshold; investigate)"
  exit 0
fi
