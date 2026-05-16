#!/usr/bin/env bash
# Effective-placement end-to-end smoke harness (US-006 of the
# ralph/effective-placement cycle — closes ROADMAP P2 added in
# commit aa83664).
#
# Drives four operator scenarios from scripts/ralph/prd.json against a
# running `multi-cluster` compose profile
# (`docker compose --profile multi-cluster up -d`). Exits non-zero with
# `FAIL: <scenario:step>` on any assertion miss; exits 0 when every
# scenario is green.
#
# Scenarios:
#   A. Weighted auto-fallback (silent migration via cluster weights):
#      - Seed bucket `ep-a-weighted` with Placement={cephb:1} mode=weighted.
#      - Seed 3 small objects → all PUT 200; chunks land on cephb.
#      - POST /drain cephb {mode:"evacuate"} → 204; state=evacuating.
#      - GET /drain-impact → assert migratable_chunks>0,
#        stuck_single_policy_chunks=0 for THIS bucket (the cluster-weights
#        fallback to `default` covers the all-draining policy).
#      - Wait for /drain-progress to report deregister_ready=true → cluster
#        converges, chunks migrated to `default`.
#      - PUT a new object on the same bucket mid-drain → 200 (auto-routed
#        to `default` via EffectivePolicy weighted-fallback).
#      - Undrain to reset for B.
#
#   B. Strict blocks drain (compliance pin):
#      - Seed bucket `ep-b-strict` with Placement={cephb:1} mode=strict.
#      - PUT 3 small objects → 200; chunks land on cephb.
#      - POST /drain cephb {mode:"evacuate"} → 204 (server doesn't gate;
#        UI gates via stuck>0 — we still flip cluster to evacuating).
#      - GET /drain-impact → assert stuck_single_policy_chunks>=1 AND
#        bucket appears in by_bucket with placement_mode="strict".
#      - PUT new object on the strict bucket mid-drain → 503 DrainRefused
#        (strict mode + all-clusters-in-policy draining → no fallback).
#      - drain-progress.deregister_ready stays false (chunks pinned).
#      - Undrain to reset for C.
#
#   C. Flip strict→weighted resolves stuck:
#      - Same bucket as B (still `ep-b-strict`); first re-drain cephb.
#      - Verify stuck_single_policy_chunks>=1 → confirm starting state.
#      - PUT /buckets/<name>/placement {placement: {cephb:1}, mode:"weighted"}
#        → 204 (audit row admin:UpdateBucketPlacementMode).
#      - GET /drain-impact immediately → stuck_single_policy_chunks==0
#        (cache invalidated on placement PUT).
#      - Wait for /drain-progress deregister_ready=true.
#      - PUT new object → 200 (weighted fallback to `default`).
#      - Undrain to reset for D.
#
#   D. All clusters drained → 503:
#      - POST /drain `default` {mode:"readonly"} → 204.
#      - POST /drain cephb {mode:"readonly"} → 204.
#      - Seed bucket `ep-d-nopolicy` (NO Placement; relies on cluster
#        weights).
#      - PUT a new object → 503 DrainRefused (every live target draining +
#        no fallback in cluster.weights left).
#      - Undrain both for clean-up.
#
# Per-scenario log lines via `echo "==> Scenario X: ..."`.
# Script EXITS NON-ZERO on any assertion failure.
#
# Pre-requisites on the host:
#   docker, curl, jq, aws (>= 2)
#   STRATA_STATIC_CREDENTIALS=<access:secret[:owner]>... exported with the
#   same value the gateway booted with (first comma-separated entry used
#   for admin login + SigV4-signed bucket setup).
#
# Lab assumptions:
#   - strata-multi gateway listens on http://127.0.0.1:9998
#   - STRATA_RADOS_CLUSTERS=default:...,cephb:...
#   - STRATA_RADOS_CLASSES leaves the STANDARD class WITHOUT an
#     `@cluster` pin so default routing actually consults weights (lab
#     default `STRATA_RADOS_CLASSES=""` is fine).
#
# Skip behavior: when the multi-cluster profile is NOT up (probe on
# /readyz fails after WAIT_GRACE seconds), the script EXITs 77
# (skipped) unless REQUIRE_LAB=1.

set -euo pipefail

BASE="${BASE:-http://127.0.0.1:9998}"
WAIT_GRACE="${WAIT_GRACE:-15}"
DRAIN_TIMEOUT_S="${DRAIN_TIMEOUT_S:-180}"
CLUSTER="${SMOKE_EP_CLUSTER:-cephb}"
OTHER_CLUSTER="${SMOKE_EP_OTHER:-default}"
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

for tool in curl jq aws; do
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
  # Best-effort undrain so a failed run doesn't leave the lab stuck.
  curl -sf -b "$JAR" -X POST "$BASE/admin/v1/clusters/$CLUSTER/undrain" \
    >/dev/null 2>&1 || true
  curl -sf -b "$JAR" -X POST "$BASE/admin/v1/clusters/$OTHER_CLUSTER/undrain" \
    >/dev/null 2>&1 || true
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

put_placement_with_mode() {
  local bucket="$1" policy="$2" mode="${3:-}"
  local body
  if [[ -n "$mode" ]]; then
    body="{\"placement\":$policy,\"mode\":\"$mode\"}"
  else
    body="{\"placement\":$policy}"
  fi
  local code
  code=$(curl -sS -o "$TMP/placement.out" -w '%{http_code}' \
    -b "$JAR" -H 'Content-Type: application/json' \
    -X PUT -d "$body" \
    "$BASE/admin/v1/buckets/$bucket/placement")
  [[ "$code" == "200" || "$code" == "204" ]] \
    || fail "PUT placement $bucket (mode=$mode): HTTP $code body=$(cat "$TMP/placement.out")"
}

cluster_state() {
  local id="$1"
  admin_get "/admin/v1/clusters" \
    | jq -r --arg id "$id" '.clusters[] | select(.id==$id) | .state'
}

drain_progress() {
  admin_get "/admin/v1/clusters/$CLUSTER/drain-progress"
}

deregister_ready() {
  drain_progress | jq -r '.deregister_ready // false'
}

wait_deregister_ready() {
  local scenario="$1"
  local deadline=$(( $(date +%s) + DRAIN_TIMEOUT_S ))
  while (( $(date +%s) < deadline )); do
    if [[ "$(deregister_ready)" == "true" ]]; then return 0; fi
    sleep 5
  done
  fail "$scenario:deregister_ready stayed false after ${DRAIN_TIMEOUT_S}s (drain_progress=$(drain_progress))"
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
dd if=/dev/urandom of="$PAYLOAD" bs=4096 count=1 2>/dev/null

# ===================================================================
# Scenario A — weighted auto-fallback
# ===================================================================
banner "Scenario A: weighted bucket — drain auto-falls back via cluster.weights"

A_BUCKET="ep-a-weighted-$STAMP"
aws --endpoint-url "$BASE" s3api create-bucket --bucket "$A_BUCKET" >/dev/null \
  || fail "A:create-bucket $A_BUCKET failed"
CLEANUP_BUCKETS+=("$A_BUCKET")
put_placement_with_mode "$A_BUCKET" "{\"$CLUSTER\":1}" "weighted"

for i in 1 2 3; do
  aws --endpoint-url "$BASE" s3 cp "$PAYLOAD" "s3://$A_BUCKET/seed-$i.bin" >/dev/null \
    || fail "A:seed PUT $i failed"
done
pass "A:seed 3 objects on weighted bucket Placement={$CLUSTER:1}"

note "drain $CLUSTER mode=evacuate"
CODE=$(admin_post_body "/admin/v1/clusters/$CLUSTER/drain" '{"mode":"evacuate"}')
[[ "$CODE" == "200" || "$CODE" == "204" ]] \
  || fail "A:drain expected 200/204 got $CODE body=$(cat "$TMP/admin.out")"
[[ "$(cluster_state "$CLUSTER")" == "evacuating" ]] \
  || fail "A:state expected evacuating got '$(cluster_state "$CLUSTER")'"
pass "A:cluster state=evacuating"

note "/drain-impact — assert migratable>0 AND stuck_single_policy=0 for the weighted bucket"
IMPACT_A=$(admin_get "/admin/v1/clusters/$CLUSTER/drain-impact")
MIG_A=$(echo "$IMPACT_A" | jq -r '.migratable_chunks // 0')
STUCK_SP_A=$(echo "$IMPACT_A" | jq -r '.stuck_single_policy_chunks // 0')
note "impact: migratable=$MIG_A stuck_single_policy=$STUCK_SP_A"
(( MIG_A > 0 )) \
  || fail "A:impact expected migratable>0 got $MIG_A (weighted fallback should classify chunks as migratable; body=$IMPACT_A)"
A_BUCKET_CAT=$(echo "$IMPACT_A" \
  | jq -r --arg n "$A_BUCKET" '.by_bucket[] | select(.name==$n) | .category // empty')
if [[ "$A_BUCKET_CAT" != "" ]]; then
  [[ "$A_BUCKET_CAT" == "migratable" ]] \
    || fail "A:bucket $A_BUCKET expected category=migratable got '$A_BUCKET_CAT' (body=$IMPACT_A)"
fi
pass "A:/drain-impact classifies weighted single-cluster bucket as migratable"

note "PUT mid-drain on weighted bucket — should auto-route via cluster.weights → 200"
PUT_CODE=$(curl -sS -o "$TMP/midput.out" -w '%{http_code}' \
  --user "$AK:$SK" --aws-sigv4 "aws:amz:us-east-1:s3" \
  -X PUT --data-binary "@$PAYLOAD" \
  -H 'Content-Type: application/octet-stream' \
  "$BASE/$A_BUCKET/mid-drain.bin")
[[ "$PUT_CODE" == "200" ]] \
  || fail "A:mid-drain PUT expected 200 got $PUT_CODE body=$(cat "$TMP/midput.out")"
pass "A:mid-drain PUT to weighted bucket succeeds (weighted-fallback to $OTHER_CLUSTER)"

note "wait deregister_ready=true (worker migrates the seed chunks)"
wait_deregister_ready "A"
pass "A:drain converged → deregister_ready=true"

note "undrain $CLUSTER"
CODE=$(admin_post_body "/admin/v1/clusters/$CLUSTER/undrain")
[[ "$CODE" == "200" || "$CODE" == "204" ]] \
  || fail "A:undrain expected 200/204 got $CODE body=$(cat "$TMP/admin.out")"

# ===================================================================
# Scenario B — strict blocks drain (compliance pin)
# ===================================================================
banner "Scenario B: strict bucket — drain leaves stuck_single_policy AND refuses PUT"

B_BUCKET="ep-b-strict-$STAMP"
aws --endpoint-url "$BASE" s3api create-bucket --bucket "$B_BUCKET" >/dev/null \
  || fail "B:create-bucket $B_BUCKET failed"
CLEANUP_BUCKETS+=("$B_BUCKET")
put_placement_with_mode "$B_BUCKET" "{\"$CLUSTER\":1}" "strict"

for i in 1 2 3; do
  aws --endpoint-url "$BASE" s3 cp "$PAYLOAD" "s3://$B_BUCKET/seed-$i.bin" >/dev/null \
    || fail "B:seed PUT $i failed"
done
pass "B:seed 3 objects on strict bucket Placement={$CLUSTER:1}"

note "drain $CLUSTER mode=evacuate"
CODE=$(admin_post_body "/admin/v1/clusters/$CLUSTER/drain" '{"mode":"evacuate"}')
[[ "$CODE" == "200" || "$CODE" == "204" ]] \
  || fail "B:drain expected 200/204 got $CODE body=$(cat "$TMP/admin.out")"
[[ "$(cluster_state "$CLUSTER")" == "evacuating" ]] \
  || fail "B:state expected evacuating got '$(cluster_state "$CLUSTER")'"

note "/drain-impact — assert stuck_single_policy>=1 AND by_bucket carries placement_mode=strict"
IMPACT_B=$(admin_get "/admin/v1/clusters/$CLUSTER/drain-impact")
STUCK_SP_B=$(echo "$IMPACT_B" | jq -r '.stuck_single_policy_chunks // 0')
note "impact: stuck_single_policy=$STUCK_SP_B"
(( STUCK_SP_B >= 1 )) \
  || fail "B:impact expected stuck_single_policy>=1 got $STUCK_SP_B (strict bucket all-draining should pin; body=$IMPACT_B)"
B_BUCKET_MODE=$(echo "$IMPACT_B" \
  | jq -r --arg n "$B_BUCKET" '.by_bucket[] | select(.name==$n) | .placement_mode // empty')
[[ "$B_BUCKET_MODE" == "strict" ]] \
  || fail "B:by_bucket[$B_BUCKET].placement_mode expected strict got '$B_BUCKET_MODE' (body=$IMPACT_B)"
B_BUCKET_CAT_B=$(echo "$IMPACT_B" \
  | jq -r --arg n "$B_BUCKET" '.by_bucket[] | select(.name==$n) | .category // empty')
[[ "$B_BUCKET_CAT_B" == "stuck_single_policy" ]] \
  || fail "B:by_bucket[$B_BUCKET].category expected stuck_single_policy got '$B_BUCKET_CAT_B' (body=$IMPACT_B)"
pass "B:/drain-impact pins strict bucket as stuck_single_policy"

note "PUT mid-drain on strict bucket — should 503 DrainRefused"
PUT_CODE=$(curl -sS -o "$TMP/strictput.out" -w '%{http_code}' \
  --user "$AK:$SK" --aws-sigv4 "aws:amz:us-east-1:s3" \
  -X PUT --data-binary "@$PAYLOAD" \
  -H 'Content-Type: application/octet-stream' \
  "$BASE/$B_BUCKET/strict-new.bin")
[[ "$PUT_CODE" == "503" ]] \
  || fail "B:strict PUT expected 503 got $PUT_CODE body=$(cat "$TMP/strictput.out")"
if ! grep -q 'DrainRefused' "$TMP/strictput.out"; then
  fail "B:strict PUT body lacks DrainRefused: $(cat "$TMP/strictput.out")"
fi
pass "B:strict PUT mid-drain returns 503 DrainRefused (no weighted fallback)"

note "drain-progress.deregister_ready stays false (strict chunks pinned)"
DEREG_B=$(deregister_ready)
[[ "$DEREG_B" != "true" ]] \
  || fail "B:expected deregister_ready=false while strict bucket has chunks on cluster; got true"
pass "B:strict bucket blocks deregister_ready"

note "undrain $CLUSTER"
CODE=$(admin_post_body "/admin/v1/clusters/$CLUSTER/undrain")
[[ "$CODE" == "200" || "$CODE" == "204" ]] \
  || fail "B:undrain expected 200/204 got $CODE body=$(cat "$TMP/admin.out")"

# ===================================================================
# Scenario C — flip strict→weighted resolves stuck
# ===================================================================
banner "Scenario C: flip strict bucket to weighted — stuck=0, drain converges"

note "re-drain $CLUSTER mode=evacuate (strict bucket from B still pinned)"
CODE=$(admin_post_body "/admin/v1/clusters/$CLUSTER/drain" '{"mode":"evacuate"}')
[[ "$CODE" == "200" || "$CODE" == "204" ]] \
  || fail "C:redrain expected 200/204 got $CODE body=$(cat "$TMP/admin.out")"

note "/drain-impact baseline — expect stuck_single_policy>=1"
IMPACT_C0=$(admin_get "/admin/v1/clusters/$CLUSTER/drain-impact")
STUCK_SP_C0=$(echo "$IMPACT_C0" | jq -r '.stuck_single_policy_chunks // 0')
(( STUCK_SP_C0 >= 1 )) \
  || fail "C:baseline expected stuck_single_policy>=1 got $STUCK_SP_C0 (body=$IMPACT_C0)"
pass "C:baseline stuck_single_policy=$STUCK_SP_C0"

note "flip $B_BUCKET mode=weighted (PUT placement with same policy + mode=weighted)"
put_placement_with_mode "$B_BUCKET" "{\"$CLUSTER\":1}" "weighted"

note "/drain-impact after flip — stuck_single_policy should drop to 0 (cache invalidated by placement PUT)"
IMPACT_C1=$(admin_get "/admin/v1/clusters/$CLUSTER/drain-impact")
STUCK_SP_C1=$(echo "$IMPACT_C1" | jq -r '.stuck_single_policy_chunks // 0')
note "impact-after-flip: stuck_single_policy=$STUCK_SP_C1"
[[ "$STUCK_SP_C1" == "0" ]] \
  || fail "C:after-flip expected stuck_single_policy=0 got $STUCK_SP_C1 (cache invalidation? body=$IMPACT_C1)"
pass "C:flip strict→weighted clears stuck_single_policy via cache invalidation"

note "wait deregister_ready=true (chunks now migratable)"
wait_deregister_ready "C"
pass "C:drain converges → deregister_ready=true after flip"

note "PUT post-flip on the now-weighted bucket — 200 via weighted-fallback"
PUT_CODE=$(curl -sS -o "$TMP/flippedput.out" -w '%{http_code}' \
  --user "$AK:$SK" --aws-sigv4 "aws:amz:us-east-1:s3" \
  -X PUT --data-binary "@$PAYLOAD" \
  -H 'Content-Type: application/octet-stream' \
  "$BASE/$B_BUCKET/post-flip.bin")
[[ "$PUT_CODE" == "200" ]] \
  || fail "C:post-flip PUT expected 200 got $PUT_CODE body=$(cat "$TMP/flippedput.out")"
pass "C:post-flip PUT lands on $OTHER_CLUSTER via weighted-fallback"

note "undrain $CLUSTER"
CODE=$(admin_post_body "/admin/v1/clusters/$CLUSTER/undrain")
[[ "$CODE" == "200" || "$CODE" == "204" ]] \
  || fail "C:undrain expected 200/204 got $CODE body=$(cat "$TMP/admin.out")"

# ===================================================================
# Scenario D — all clusters drained → 503 (genuine no-target)
# ===================================================================
banner "Scenario D: drain ALL clusters → PUT on nil-policy bucket → 503"

note "drain $OTHER_CLUSTER mode=readonly"
CODE=$(admin_post_body "/admin/v1/clusters/$OTHER_CLUSTER/drain" '{"mode":"readonly"}')
[[ "$CODE" == "200" || "$CODE" == "204" ]] \
  || fail "D:drain $OTHER_CLUSTER expected 200/204 got $CODE body=$(cat "$TMP/admin.out")"

note "drain $CLUSTER mode=readonly"
CODE=$(admin_post_body "/admin/v1/clusters/$CLUSTER/drain" '{"mode":"readonly"}')
[[ "$CODE" == "200" || "$CODE" == "204" ]] \
  || fail "D:drain $CLUSTER expected 200/204 got $CODE body=$(cat "$TMP/admin.out")"

D_BUCKET="ep-d-nopolicy-$STAMP"
aws --endpoint-url "$BASE" s3api create-bucket --bucket "$D_BUCKET" >/dev/null \
  || fail "D:create-bucket $D_BUCKET failed"
CLEANUP_BUCKETS+=("$D_BUCKET")

note "PUT to nil-policy bucket with all clusters drained — expect 503 DrainRefused"
PUT_CODE=$(curl -sS -o "$TMP/alldrained.out" -w '%{http_code}' \
  --user "$AK:$SK" --aws-sigv4 "aws:amz:us-east-1:s3" \
  -X PUT --data-binary "@$PAYLOAD" \
  -H 'Content-Type: application/octet-stream' \
  "$BASE/$D_BUCKET/all-drained.bin")
[[ "$PUT_CODE" == "503" ]] \
  || fail "D:all-drained PUT expected 503 got $PUT_CODE body=$(cat "$TMP/alldrained.out")"
if ! grep -q 'DrainRefused' "$TMP/alldrained.out"; then
  fail "D:all-drained PUT body lacks DrainRefused: $(cat "$TMP/alldrained.out")"
fi
pass "D:all-drained PUT returns 503 DrainRefused (no fallback target anywhere)"

note "undrain both clusters"
CODE=$(admin_post_body "/admin/v1/clusters/$CLUSTER/undrain")
[[ "$CODE" == "200" || "$CODE" == "204" ]] \
  || fail "D:undrain $CLUSTER expected 200/204 got $CODE body=$(cat "$TMP/admin.out")"
CODE=$(admin_post_body "/admin/v1/clusters/$OTHER_CLUSTER/undrain")
[[ "$CODE" == "200" || "$CODE" == "204" ]] \
  || fail "D:undrain $OTHER_CLUSTER expected 200/204 got $CODE body=$(cat "$TMP/admin.out")"

echo
echo "== effective-placement smoke OK (Scenarios A + B + C + D green)"
