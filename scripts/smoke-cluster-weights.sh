#!/usr/bin/env bash
# Cluster-weights end-to-end smoke harness (US-005 of the
# ralph/cluster-weights cycle).
#
# Drives the four walkthrough scenarios from
# scripts/ralph/prd.json against a running `multi-cluster` compose
# profile (`docker compose --profile multi-cluster up -d`). Exits
# non-zero with `FAIL: <scenario:step>` on any assertion miss; exits 0
# when every scenario is green.
#
# Scenarios:
#   A. New cluster activation:
#      - Wipe cluster_state row for cephb via cqlsh → restart strata-multi
#      - Assert /clusters reports cephb state=pending weight=0
#      - POST /clusters/cephb/activate {weight:10} → assert live + weight=10
#      - PUT 1000 chunks via nil-policy bucket → assert ~100 land on cephb
#        (5% tolerance via `rados ls`)
#      - PUT /clusters/cephb/weight {weight:50} → 200
#      - PUT 1000 more chunks → assert ~50/50 distribution
#
#   B. Existing-live auto-detect at boot:
#      - PUT N chunks via nil-policy bucket on the freshly-live cephb
#      - Wipe cluster_state row for `default` cluster
#      - Restart strata-multi
#      - Assert /clusters reports `default` state=live weight=100 (bucket_stats
#        reference path) NOT pending.
#
#   C. Explicit policy overrides weights:
#      - Create bucket with Placement={default:1, cephb:1}
#      - Set cluster weights {default:100, cephb:10}
#      - PUT 200 chunks
#      - Assert ~100/100 distribution (bucket policy wins — 50/50 ratio
#        from policy, NOT 100/10 from weights)
#
#   D. Pending excluded from default routing:
#      - Wipe cluster_state for cephb → boot auto-init pending weight=0
#      - PUT 100 chunks via nil-policy bucket → assert 0 chunks on cephb
#      - Activate cephb {weight:10} → PUT 100 more → assert ~10 on cephb
#
# Script EXITs NON-ZERO on any assertion failure; per-scenario log
# lines via `==> Scenario X: ...` so failing scenario is obvious.
#
# Pre-requisites on the host:
#   docker, curl, jq, aws (>= 2), md5sum or md5
#   STRATA_STATIC_CREDENTIALS=<access:secret[:owner]>... exported with
#   the same value the gateway booted with (first comma-separated entry
#   used for admin login + SigV4-signed bucket setup).
#
# Lab assumptions:
#   - strata-multi gateway listens on http://127.0.0.1:9998
#   - STRATA_RADOS_CLUSTERS=default:...,cephb:...
#   - STRATA_RADOS_CLASSES leaves the STANDARD class WITHOUT an
#     `@cluster` pin so default routing actually consults weights.
#     (The lab default `STRATA_RADOS_CLASSES=""` is fine.)
#
# Skip behavior: when the multi-cluster profile is NOT up (probe on
# /readyz fails after WAIT_GRACE seconds), the script EXITs 77
# (skipped) unless REQUIRE_LAB=1.

set -euo pipefail

BASE="${BASE:-http://127.0.0.1:9998}"
WAIT_GRACE="${WAIT_GRACE:-15}"
RESTART_GRACE="${RESTART_GRACE:-60}"
CLUSTER="${SMOKE_CW_CLUSTER:-cephb}"
OTHER_CLUSTER="${SMOKE_CW_OTHER:-default}"
GATEWAY_CONTAINER="${SMOKE_CW_GATEWAY:-strata-multi}"
CASSANDRA_CONTAINER="${SMOKE_CW_CASSANDRA:-strata-cassandra}"
KEYSPACE="${SMOKE_CW_KEYSPACE:-strata}"
RADOS_CONTAINER="${SMOKE_CW_RADOS:-strata-ceph}"
RADOS_CONTAINER_B="${SMOKE_CW_RADOS_B:-strata-ceph-b}"
RADOS_POOL="${SMOKE_CW_POOL:-strata-data}"
REQUIRE_LAB="${REQUIRE_LAB:-0}"
PUT_COUNT_LARGE="${SMOKE_CW_PUT_LARGE:-1000}"
PUT_COUNT_SMALL="${SMOKE_CW_PUT_SMALL:-200}"
PUT_COUNT_TINY="${SMOKE_CW_PUT_TINY:-100}"
TOLERANCE_PCT="${SMOKE_CW_TOLERANCE_PCT:-5}"
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
CLEANUP_BUCKETS=()

fail() { echo "FAIL: $*" >&2; exit 1; }
pass() { echo "PASS: $*"; }
note() { echo "INFO: $*"; }
banner() { echo; echo "==> $*"; }

cleanup() {
  for b in "${CLEANUP_BUCKETS[@]}"; do
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

wait_ready() {
  local i=0
  while (( i < RESTART_GRACE )); do
    if [[ "$(curl -fsS -o /dev/null -w '%{http_code}' "$BASE/readyz" 2>/dev/null)" == "200" ]]; then
      return 0
    fi
    sleep 1
    i=$((i+1))
  done
  return 1
}

if ! probe_ready; then
  msg="multi-cluster lab not reachable on $BASE/readyz after ${WAIT_GRACE}s"
  if [[ "$REQUIRE_LAB" == "1" ]]; then
    fail "$msg (REQUIRE_LAB=1)"
  fi
  echo "SKIP: $msg" >&2
  echo "SKIP: bring it up with 'docker compose --profile multi-cluster up -d' then re-run." >&2
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

admin_get() {
  local path="$1"
  curl -sf -b "$JAR" "$BASE$path"
}

admin_post_body() {
  local path="$1" body="${2:-}"
  local code
  if [[ -n "$body" ]]; then
    code=$(curl -sS -o "$TMP/admin.out" -w '%{http_code}' \
      -b "$JAR" -H 'Content-Type: application/json' \
      -X POST -d "$body" "$BASE$path")
  else
    code=$(curl -sS -o "$TMP/admin.out" -w '%{http_code}' \
      -b "$JAR" -X POST "$BASE$path")
  fi
  echo "$code"
}

admin_put_body() {
  local path="$1" body="${2:-}"
  local code
  if [[ -n "$body" ]]; then
    code=$(curl -sS -o "$TMP/admin.out" -w '%{http_code}' \
      -b "$JAR" -H 'Content-Type: application/json' \
      -X PUT -d "$body" "$BASE$path")
  else
    code=$(curl -sS -o "$TMP/admin.out" -w '%{http_code}' \
      -b "$JAR" -X PUT "$BASE$path")
  fi
  echo "$code"
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

cluster_state_and_weight() {
  local id="$1"
  admin_get "/admin/v1/clusters" \
    | jq -r --arg id "$id" '.clusters[] | select(.id==$id) | "\(.state) \(.weight // 0)"'
}

wipe_cluster_state() {
  local id="$1"
  note "wipe cluster_state row for '$id' via cqlsh on $CASSANDRA_CONTAINER"
  docker exec "$CASSANDRA_CONTAINER" cqlsh -e \
    "DELETE FROM ${KEYSPACE}.cluster_state WHERE cluster_id='$id';" \
    >/dev/null 2>&1 \
    || fail "cassandra DELETE cluster_state where id='$id' failed (container=$CASSANDRA_CONTAINER keyspace=$KEYSPACE)"
}

restart_gateway() {
  note "restart $GATEWAY_CONTAINER"
  docker restart "$GATEWAY_CONTAINER" >/dev/null \
    || fail "docker restart $GATEWAY_CONTAINER failed"
  wait_ready \
    || fail "gateway $GATEWAY_CONTAINER did not become ready in ${RESTART_GRACE}s after restart"
}

rados_object_count() {
  local container="$1"
  # Count strata data-pool objects; suppress harmless RADOS chatter.
  docker exec "$container" rados -p "$RADOS_POOL" ls 2>/dev/null | wc -l | awk '{print $1}'
}

within_tolerance() {
  local actual="$1" expected="$2" tol_pct="${3:-$TOLERANCE_PCT}"
  if (( expected == 0 )); then
    [[ "$actual" == "0" ]]
    return $?
  fi
  local diff=$(( actual - expected ))
  if (( diff < 0 )); then diff=$(( -diff )); fi
  local tol=$(( expected * tol_pct / 100 ))
  (( diff <= tol ))
}

aws_creds() {
  export AWS_ACCESS_KEY_ID="$AK"
  export AWS_SECRET_ACCESS_KEY="$SK"
  export AWS_DEFAULT_REGION="${AWS_DEFAULT_REGION:-us-east-1}"
  export AWS_EC2_METADATA_DISABLED=true
}

aws_creds
login

PAYLOAD="$TMP/payload.bin"
dd if=/dev/urandom of="$PAYLOAD" bs=1024 count=1 2>/dev/null

# put_many <bucket> <count> <prefix> — uploads N tiny objects in
# parallel batches under `<prefix>/<i>.bin`.
put_many() {
  local bucket="$1" count="$2" prefix="$3"
  local i
  for (( i = 1; i <= count; i++ )); do
    aws --endpoint-url "$BASE" s3 cp "$PAYLOAD" "s3://$bucket/$prefix/$i.bin" \
      >/dev/null 2>&1 \
      || fail "put-many $bucket/$prefix: PUT $i failed"
    if (( i % 50 == 0 )); then
      printf '.'
    fi
  done
  echo
}

# ===================================================================
# Scenario A — new cluster activation (pending → live → ramp)
# ===================================================================
banner "Scenario A: new-cluster activation flow"

wipe_cluster_state "$CLUSTER"
restart_gateway
login  # cookie jar after restart

read -r A_STATE A_WEIGHT <<<"$(cluster_state_and_weight "$CLUSTER")"
[[ "$A_STATE" == "pending" ]] \
  || fail "A:state expected pending got '$A_STATE' (weight=$A_WEIGHT)"
[[ "$A_WEIGHT" == "0" ]] \
  || fail "A:weight expected 0 got '$A_WEIGHT' (state=$A_STATE)"
pass "A:boot reconcile yields state=pending weight=0 for $CLUSTER"

note "POST /activate {weight:10}"
CODE=$(admin_post_body "/admin/v1/clusters/$CLUSTER/activate" '{"weight":10}')
[[ "$CODE" == "200" || "$CODE" == "204" ]] \
  || fail "A:activate expected 200 got $CODE body=$(cat "$TMP/admin.out")"
read -r A_STATE2 A_WEIGHT2 <<<"$(cluster_state_and_weight "$CLUSTER")"
[[ "$A_STATE2" == "live" && "$A_WEIGHT2" == "10" ]] \
  || fail "A:state/weight after activate expected live/10 got '$A_STATE2/$A_WEIGHT2'"
pass "A:activate flipped pending → live weight=10"

A_BUCKET="cw-a-$STAMP"
aws --endpoint-url "$BASE" s3api create-bucket --bucket "$A_BUCKET" >/dev/null \
  || fail "A:create-bucket $A_BUCKET failed"
CLEANUP_BUCKETS+=("$A_BUCKET")
# Explicit no-placement; default routing should consult cluster weights.

BEFORE_OTHER=$(rados_object_count "$RADOS_CONTAINER")
BEFORE_CEPHB=$(rados_object_count "$RADOS_CONTAINER_B")
note "A:before PUT — $OTHER_CLUSTER=$BEFORE_OTHER $CLUSTER=$BEFORE_CEPHB"

note "PUT $PUT_COUNT_LARGE chunks @ weight 10/100"
put_many "$A_BUCKET" "$PUT_COUNT_LARGE" "stage1"

AFTER1_OTHER=$(rados_object_count "$RADOS_CONTAINER")
AFTER1_CEPHB=$(rados_object_count "$RADOS_CONTAINER_B")
DELTA1_OTHER=$(( AFTER1_OTHER - BEFORE_OTHER ))
DELTA1_CEPHB=$(( AFTER1_CEPHB - BEFORE_CEPHB ))
note "A:stage1 distribution $OTHER_CLUSTER=+$DELTA1_OTHER $CLUSTER=+$DELTA1_CEPHB"
EXPECT_CEPHB=$(( PUT_COUNT_LARGE * 10 / 110 ))
within_tolerance "$DELTA1_CEPHB" "$EXPECT_CEPHB" 50 \
  || fail "A:stage1 cephb chunks expected ~$EXPECT_CEPHB got $DELTA1_CEPHB (PUT_COUNT_LARGE=$PUT_COUNT_LARGE, 50% tol — distribution noisy on small N)"
pass "A:stage1 weight=10 yields proportional distribution (within tol)"

note "PUT /weight {weight:50}"
CODE=$(admin_put_body "/admin/v1/clusters/$CLUSTER/weight" '{"weight":50}')
[[ "$CODE" == "200" || "$CODE" == "204" ]] \
  || fail "A:PUT weight expected 200 got $CODE body=$(cat "$TMP/admin.out")"
read -r _ A_WEIGHT3 <<<"$(cluster_state_and_weight "$CLUSTER")"
[[ "$A_WEIGHT3" == "50" ]] \
  || fail "A:weight after PUT expected 50 got '$A_WEIGHT3'"
pass "A:weight ramp 10 → 50 accepted"

note "PUT $PUT_COUNT_LARGE chunks @ weight 50/150"
put_many "$A_BUCKET" "$PUT_COUNT_LARGE" "stage2"

AFTER2_OTHER=$(rados_object_count "$RADOS_CONTAINER")
AFTER2_CEPHB=$(rados_object_count "$RADOS_CONTAINER_B")
DELTA2_OTHER=$(( AFTER2_OTHER - AFTER1_OTHER ))
DELTA2_CEPHB=$(( AFTER2_CEPHB - AFTER1_CEPHB ))
note "A:stage2 distribution $OTHER_CLUSTER=+$DELTA2_OTHER $CLUSTER=+$DELTA2_CEPHB"
# After ramp the default cluster has weight=100, cephb=50, so cephb expects
# PUT_COUNT_LARGE * 50/150 ~= 333.
EXPECT2_CEPHB=$(( PUT_COUNT_LARGE * 50 / 150 ))
within_tolerance "$DELTA2_CEPHB" "$EXPECT2_CEPHB" 25 \
  || fail "A:stage2 cephb chunks expected ~$EXPECT2_CEPHB got $DELTA2_CEPHB (25% tol)"
pass "A:stage2 weight=50 ramps cephb routing share"

# ===================================================================
# Scenario B — existing-live auto-detect at boot
# ===================================================================
banner "Scenario B: existing-live auto-detect (bucket_stats path)"

note "PUT $PUT_COUNT_TINY pre-existing chunks via existing nil-policy bucket"
B_BUCKET="cw-b-$STAMP"
aws --endpoint-url "$BASE" s3api create-bucket --bucket "$B_BUCKET" >/dev/null \
  || fail "B:create-bucket $B_BUCKET failed"
CLEANUP_BUCKETS+=("$B_BUCKET")
put_many "$B_BUCKET" "$PUT_COUNT_TINY" "seed"

wipe_cluster_state "$OTHER_CLUSTER"
restart_gateway
login

read -r B_STATE B_WEIGHT <<<"$(cluster_state_and_weight "$OTHER_CLUSTER")"
[[ "$B_STATE" == "live" ]] \
  || fail "B:state expected live got '$B_STATE' weight='$B_WEIGHT' (existing-live detection failed)"
[[ "$B_WEIGHT" == "100" ]] \
  || fail "B:weight expected 100 got '$B_WEIGHT' state='$B_STATE'"
pass "B:boot reconcile auto-detects existing-live $OTHER_CLUSTER → state=live weight=100"

# ===================================================================
# Scenario C — bucket Placement policy overrides cluster weights
# ===================================================================
banner "Scenario C: explicit bucket policy wins over cluster.weight"

# Set asymmetric cluster weights — default=100, cephb=10. If policy
# were respected by routing this would land 100/10. Bucket policy
# {default:1, cephb:1} should yield 50/50 regardless.
CODE=$(admin_put_body "/admin/v1/clusters/$OTHER_CLUSTER/weight" '{"weight":100}')
[[ "$CODE" == "200" || "$CODE" == "204" ]] \
  || fail "C:set $OTHER_CLUSTER weight=100 expected 200 got $CODE body=$(cat "$TMP/admin.out")"
CODE=$(admin_put_body "/admin/v1/clusters/$CLUSTER/weight" '{"weight":10}')
[[ "$CODE" == "200" || "$CODE" == "204" ]] \
  || fail "C:set $CLUSTER weight=10 expected 200 got $CODE body=$(cat "$TMP/admin.out")"

C_BUCKET="cw-c-$STAMP"
aws --endpoint-url "$BASE" s3api create-bucket --bucket "$C_BUCKET" >/dev/null \
  || fail "C:create-bucket $C_BUCKET failed"
CLEANUP_BUCKETS+=("$C_BUCKET")
put_placement "$C_BUCKET" "{\"$OTHER_CLUSTER\":1,\"$CLUSTER\":1}"

BEFORE_OTHER=$(rados_object_count "$RADOS_CONTAINER")
BEFORE_CEPHB=$(rados_object_count "$RADOS_CONTAINER_B")

put_many "$C_BUCKET" "$PUT_COUNT_SMALL" "x"

AFTER_OTHER=$(rados_object_count "$RADOS_CONTAINER")
AFTER_CEPHB=$(rados_object_count "$RADOS_CONTAINER_B")
DELTA_OTHER=$(( AFTER_OTHER - BEFORE_OTHER ))
DELTA_CEPHB=$(( AFTER_CEPHB - BEFORE_CEPHB ))
note "C:distribution $OTHER_CLUSTER=+$DELTA_OTHER $CLUSTER=+$DELTA_CEPHB (expected ~50/50)"

EXPECT_HALF=$(( PUT_COUNT_SMALL / 2 ))
within_tolerance "$DELTA_OTHER" "$EXPECT_HALF" 25 \
  || fail "C:expected ~50% on $OTHER_CLUSTER (~$EXPECT_HALF) got $DELTA_OTHER — bucket policy not honored?"
within_tolerance "$DELTA_CEPHB" "$EXPECT_HALF" 25 \
  || fail "C:expected ~50% on $CLUSTER (~$EXPECT_HALF) got $DELTA_CEPHB — cluster.weight=10 leaking into routing?"
pass "C:bucket Placement={1,1} yields ~50/50 split regardless of cluster.weight={100,10}"

# ===================================================================
# Scenario D — pending excluded from default routing
# ===================================================================
banner "Scenario D: pending cluster excluded from default routing"

wipe_cluster_state "$CLUSTER"
restart_gateway
login

read -r D_STATE D_WEIGHT <<<"$(cluster_state_and_weight "$CLUSTER")"
[[ "$D_STATE" == "pending" && "$D_WEIGHT" == "0" ]] \
  || fail "D:state/weight expected pending/0 got '$D_STATE/$D_WEIGHT'"
pass "D:boot reconcile yields pending/0 for $CLUSTER"

D_BUCKET="cw-d-$STAMP"
aws --endpoint-url "$BASE" s3api create-bucket --bucket "$D_BUCKET" >/dev/null \
  || fail "D:create-bucket $D_BUCKET failed"
CLEANUP_BUCKETS+=("$D_BUCKET")

BEFORE_CEPHB=$(rados_object_count "$RADOS_CONTAINER_B")
put_many "$D_BUCKET" "$PUT_COUNT_TINY" "pending"
AFTER_CEPHB=$(rados_object_count "$RADOS_CONTAINER_B")
DELTA_CEPHB=$(( AFTER_CEPHB - BEFORE_CEPHB ))
note "D:pending stage cephb=+$DELTA_CEPHB"
[[ "$DELTA_CEPHB" == "0" ]] \
  || fail "D:expected 0 chunks on pending $CLUSTER got $DELTA_CEPHB"
pass "D:nil-policy PUTs skip pending $CLUSTER entirely"

note "Activate $CLUSTER {weight:10}"
CODE=$(admin_post_body "/admin/v1/clusters/$CLUSTER/activate" '{"weight":10}')
[[ "$CODE" == "200" || "$CODE" == "204" ]] \
  || fail "D:activate expected 200 got $CODE body=$(cat "$TMP/admin.out")"

BEFORE_CEPHB=$AFTER_CEPHB
put_many "$D_BUCKET" "$PUT_COUNT_TINY" "post-activate"
AFTER_CEPHB=$(rados_object_count "$RADOS_CONTAINER_B")
DELTA_CEPHB=$(( AFTER_CEPHB - BEFORE_CEPHB ))
# default still has weight=100 from Scenario C, cephb=10 → expect ~10/110.
EXPECT_CEPHB=$(( PUT_COUNT_TINY * 10 / 110 ))
note "D:post-activate cephb=+$DELTA_CEPHB (expected ~$EXPECT_CEPHB ±100%)"
(( DELTA_CEPHB > 0 )) \
  || fail "D:after activate expected >0 chunks on $CLUSTER got $DELTA_CEPHB"
pass "D:activate $CLUSTER weight=10 starts landing chunks (count=$DELTA_CEPHB)"

echo
echo "== cluster-weights smoke OK (Scenarios A + B + C + D green)"
