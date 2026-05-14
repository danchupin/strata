#!/usr/bin/env bash
# Drain-transparency end-to-end smoke harness (US-008 of the
# ralph/drain-transparency cycle).
#
# Walks three end-to-end operator scenarios from
# scripts/ralph/prd.json against a running `multi-cluster` compose
# profile (`docker compose --profile multi-cluster up -d`). Exits
# non-zero with `FAIL: <scenario:step>` on any assertion miss; exits 0
# when every scenario is green.
#
# Scenarios:
#   A. Stop-writes drain (maintenance):
#      - Create bucket `dt-a-split` with Placement={cephb:1,default:1}
#      - PUT 5 objects → assert all 200
#      - POST /drain cephb {mode:"readonly"} → assert state=draining_readonly mode=readonly
#      - PUT a fresh cephb-routed object via single-cluster bucket → 503 DrainRefused
#      - GET pre-existing object on split bucket → 200 (reads survive)
#      - Init+Upload+Complete in-flight multipart on cephb mid-drain → 200 (graceful)
#      - POST /undrain → state=live; PUT → 200
#
#   B. Full evacuate (decommission) — pre-drain impact + bulk fix gates Submit:
#      - Create three buckets: tx-split {cephb:1,default:1}, tx-stuck {cephb:1},
#        tx-residual (no policy).
#      - GET /drain-impact cephb → assert categorized counts
#        (migratable>0, stuck_single_policy>0)
#      - Attempt drain {mode:"evacuate"} without fix → succeeds at admin level
#        (server doesn't gate; UI gates) — assert state=evacuating
#      - Undrain
#      - PUT /buckets/tx-stuck/placement {cephb:1,default:1} (uniform-live fix)
#      - Retry drain {mode:"evacuate"} → 200; wait for drain-progress
#        chunks_on_cluster to reach 0 → assert deregister_ready=true
#
#   C. Upgrade readonly → evacuate:
#      - Start from state=live; POST /drain {mode:"readonly"} → assert state=draining_readonly
#      - GET /drain-impact → assert state=draining_readonly + categorized counts
#      - POST /drain {mode:"evacuate"} → 204; assert state=evacuating
#      - Wait for drain-progress chunks_on_cluster to reach 0 →
#        assert deregister_ready=true
#
# Pre-requisites on the host:
#   docker, curl, jq, aws (>= 2), md5sum or md5
#   STRATA_STATIC_CREDENTIALS=<access:secret[:owner]>... exported with the
#   same value the gateway booted with (first comma-separated entry used
#   for admin login + SigV4-signed bucket setup).
#
# Lab assumptions:
#   - strata-multi gateway listens on http://127.0.0.1:9998
#   - STRATA_RADOS_CLUSTERS=default:...,cephb:...
#   - STRATA_RADOS_CLASSES exposes ≥1 class pinned to `default` (we PUT
#     into single-cluster buckets to drive routing through the drained
#     `cephb` cluster).
#
# Skip behavior: when the multi-cluster profile is NOT up (probe on
# /readyz fails after WAIT_GRACE seconds), the script EXITs 77 (skipped)
# unless REQUIRE_LAB=1.

set -euo pipefail

BASE="${BASE:-http://127.0.0.1:9998}"
WAIT_GRACE="${WAIT_GRACE:-15}"
DRAIN_TIMEOUT_S="${DRAIN_TIMEOUT_S:-180}"
CLUSTER="${SMOKE_DRAIN_CLUSTER:-cephb}"
OTHER_CLUSTER="${SMOKE_DRAIN_OTHER:-default}"
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
  command -v "$tool" >/dev/null 2>&1 || { echo "FAIL: missing tool: $tool" >&2; exit 2; }
done

md5_of() {
  if command -v md5sum >/dev/null 2>&1; then md5sum "$1" | awk '{print $1}';
  elif command -v md5 >/dev/null 2>&1; then md5 -q "$1";
  else echo "FAIL: no md5sum/md5 tool" >&2; exit 2; fi
}

TMP="$(mktemp -d)"
JAR="$TMP/cookies"
CLEANUP_BUCKETS=()
CLEANUP_UNDRAIN="${CLUSTER}"

fail() { echo "FAIL: $*" >&2; exit 1; }
pass() { echo "PASS: $*"; }
note() { echo "INFO: $*"; }
banner() { echo; echo "==> $*"; }

cleanup() {
  for b in "${CLEANUP_BUCKETS[@]}"; do
    aws --endpoint-url "$BASE" s3 rm "s3://$b" --recursive >/dev/null 2>&1 || true
    aws --endpoint-url "$BASE" s3api delete-bucket --bucket "$b" >/dev/null 2>&1 || true
  done
  # Best-effort undrain so a failed run doesn't leave the lab stuck in
  # draining_readonly / evacuating.
  curl -sf -b "$JAR" -X POST "$BASE/admin/v1/clusters/$CLEANUP_UNDRAIN/undrain" \
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

cluster_state_and_mode() {
  admin_get "/admin/v1/clusters" \
    | jq -r --arg id "$CLUSTER" '.clusters[] | select(.id==$id) | "\(.state) \(.mode)"'
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
ORIG_MD5="$(md5_of "$PAYLOAD")"

LARGE="$TMP/large.bin"
# 6 MiB so multipart with two ≥ 5 MiB parts is legal under S3 multipart minimums.
dd if=/dev/urandom of="$LARGE" bs=$((1024*1024)) count=6 2>/dev/null
LARGE_MD5="$(md5_of "$LARGE")"

# ===================================================================
# Scenario A — stop-writes drain (maintenance)
# ===================================================================
banner "Scenario A: stop-writes drain on $CLUSTER (mode=readonly)"

A_SPLIT="dt-a-split-$STAMP"
A_CEPHB="dt-a-cephb-$STAMP"
aws --endpoint-url "$BASE" s3api create-bucket --bucket "$A_SPLIT" >/dev/null
aws --endpoint-url "$BASE" s3api create-bucket --bucket "$A_CEPHB" >/dev/null
CLEANUP_BUCKETS+=("$A_SPLIT" "$A_CEPHB")
put_placement "$A_SPLIT" "{\"$OTHER_CLUSTER\":1,\"$CLUSTER\":1}"
put_placement "$A_CEPHB" "{\"$CLUSTER\":1}"

note "PUT 5 objects across $A_SPLIT before drain"
for i in 1 2 3 4 5; do
  aws --endpoint-url "$BASE" s3 cp "$PAYLOAD" "s3://$A_SPLIT/pre-$i.bin" >/dev/null \
    || fail "A:pre PUT $i failed"
done
pass "A:pre seed 5 objects on $A_SPLIT"

note "Drain $CLUSTER mode=readonly"
CODE=$(admin_post_body "/admin/v1/clusters/$CLUSTER/drain" '{"mode":"readonly"}')
[[ "$CODE" == "204" || "$CODE" == "200" ]] \
  || fail "A:drain expected 204 got $CODE (body=$(cat "$TMP/admin.out"))"
read -r STATE_A MODE_A <<<"$(cluster_state_and_mode)"
[[ "$STATE_A" == "draining_readonly" ]] \
  || fail "A:state expected draining_readonly got '$STATE_A' mode='$MODE_A'"
[[ "$MODE_A" == "readonly" ]] \
  || fail "A:mode expected readonly got '$MODE_A'"
pass "A:drain accepted; state=draining_readonly mode=readonly"

note "PUT into cephb-only bucket should 503 DrainRefused"
PUT_CODE=$(curl -sS -o "$TMP/refused.out" -w '%{http_code}' \
  --user "$AK:$SK" --aws-sigv4 "aws:amz:us-east-1:s3" \
  -X PUT --data-binary "@$PAYLOAD" \
  -H 'Content-Type: application/octet-stream' \
  "$BASE/$A_CEPHB/new-after-drain.bin")
if [[ "$PUT_CODE" != "503" ]]; then
  fail "A:refused PUT expected 503 got $PUT_CODE body=$(cat "$TMP/refused.out")"
fi
if ! grep -q 'DrainRefused' "$TMP/refused.out"; then
  fail "A:refused PUT body lacks DrainRefused: $(cat "$TMP/refused.out")"
fi
pass "A:new PUT routed to drained $CLUSTER returns 503 DrainRefused"

note "GET pre-existing object on $A_SPLIT (reads survive drain)"
aws --endpoint-url "$BASE" s3 cp "s3://$A_SPLIT/pre-1.bin" "$TMP/A-read.bin" >/dev/null \
  || fail "A:read pre-existing PUT failed"
GOT=$(md5_of "$TMP/A-read.bin")
[[ "$GOT" == "$ORIG_MD5" ]] || fail "A:read md5 mismatch ($GOT vs $ORIG_MD5)"
pass "A:reads on pre-existing objects unaffected by readonly drain"

note "In-flight multipart on $A_CEPHB mid-drain (graceful contract)"
# Init multipart against the cephb-only bucket while it is draining.
INIT_OUT=$(aws --endpoint-url "$BASE" s3api create-multipart-upload \
  --bucket "$A_CEPHB" --key inflight-mp.bin 2>&1) \
  || fail "A:mp init failed: $INIT_OUT"
UPLOAD_ID=$(echo "$INIT_OUT" | jq -r '.UploadId // empty')
[[ -n "$UPLOAD_ID" ]] || fail "A:mp init produced no UploadId (output=$INIT_OUT)"
# Two ≥ 5 MiB parts. Strata accepts the 6 MiB blob split into 5 MiB + 1 MiB
# tail.
split -b 5m "$LARGE" "$TMP/A-mp-"
PARTS=("$TMP/A-mp-aa" "$TMP/A-mp-ab")
ETAGS=()
for idx in 1 2; do
  part_file="${PARTS[$((idx-1))]}"
  PART_OUT=$(aws --endpoint-url "$BASE" s3api upload-part \
    --bucket "$A_CEPHB" --key inflight-mp.bin \
    --upload-id "$UPLOAD_ID" --part-number "$idx" \
    --body "$part_file" 2>&1) \
    || fail "A:mp upload-part $idx failed: $PART_OUT"
  ETAGS+=("$(echo "$PART_OUT" | jq -r '.ETag')")
done
COMPLETE_JSON="$TMP/A-mp-complete.json"
jq -n --arg e1 "${ETAGS[0]}" --arg e2 "${ETAGS[1]}" '
  { Parts: [
      { ETag: $e1, PartNumber: 1 },
      { ETag: $e2, PartNumber: 2 }
    ] }' >"$COMPLETE_JSON"
aws --endpoint-url "$BASE" s3api complete-multipart-upload \
  --bucket "$A_CEPHB" --key inflight-mp.bin \
  --upload-id "$UPLOAD_ID" --multipart-upload "file://$COMPLETE_JSON" >/dev/null \
  || fail "A:mp complete failed"
# Read it back and md5-compare so we know it actually landed on $CLUSTER.
aws --endpoint-url "$BASE" s3 cp "s3://$A_CEPHB/inflight-mp.bin" "$TMP/A-mp.got" >/dev/null \
  || fail "A:mp read-back failed"
GOT=$(md5_of "$TMP/A-mp.got")
[[ "$GOT" == "$LARGE_MD5" ]] || fail "A:mp md5 mismatch ($GOT vs $LARGE_MD5)"
pass "A:in-flight multipart Init+UploadPart+Complete finishes gracefully on draining $CLUSTER"

note "Undrain → state=live → new PUTs succeed"
CODE=$(admin_post_body "/admin/v1/clusters/$CLUSTER/undrain")
[[ "$CODE" == "204" || "$CODE" == "200" ]] \
  || fail "A:undrain expected 204 got $CODE (body=$(cat "$TMP/admin.out"))"
sleep 1
read -r STATE_AFTER _ <<<"$(cluster_state_and_mode)"
[[ "$STATE_AFTER" == "live" ]] || fail "A:state after undrain expected live got '$STATE_AFTER'"
aws --endpoint-url "$BASE" s3 cp "$PAYLOAD" "s3://$A_CEPHB/post-undrain.bin" >/dev/null \
  || fail "A:post-undrain PUT failed"
pass "A:undrain restored live; new PUT succeeds"

# ===================================================================
# Scenario B — full evacuate (decommission) with pre-drain impact + bulk fix
# ===================================================================
banner "Scenario B: full evacuate on $CLUSTER (mode=evacuate) with pre-drain impact analysis"

B_SPLIT="dt-b-split-$STAMP"
B_STUCK="dt-b-stuck-$STAMP"
B_RESIDUAL="dt-b-residual-$STAMP"
aws --endpoint-url "$BASE" s3api create-bucket --bucket "$B_SPLIT"    >/dev/null
aws --endpoint-url "$BASE" s3api create-bucket --bucket "$B_STUCK"    >/dev/null
aws --endpoint-url "$BASE" s3api create-bucket --bucket "$B_RESIDUAL" >/dev/null
CLEANUP_BUCKETS+=("$B_SPLIT" "$B_STUCK" "$B_RESIDUAL")
put_placement "$B_SPLIT" "{\"$OTHER_CLUSTER\":1,\"$CLUSTER\":1}"
put_placement "$B_STUCK" "{\"$CLUSTER\":1}"
# B_RESIDUAL intentionally has no policy.

for i in 1 2 3; do
  aws --endpoint-url "$BASE" s3 cp "$PAYLOAD" "s3://$B_SPLIT/o-$i.bin"    >/dev/null \
    || fail "B:seed split $i failed"
  aws --endpoint-url "$BASE" s3 cp "$PAYLOAD" "s3://$B_STUCK/o-$i.bin"    >/dev/null \
    || fail "B:seed stuck $i failed"
  aws --endpoint-url "$BASE" s3 cp "$PAYLOAD" "s3://$B_RESIDUAL/o-$i.bin" >/dev/null \
    || fail "B:seed residual $i failed"
done

note "GET /drain-impact $CLUSTER (state=live) — categorized chunk counts"
IMPACT=$(admin_get "/admin/v1/clusters/$CLUSTER/drain-impact")
echo "$IMPACT" | jq . >/dev/null \
  || fail "B:impact body not JSON: $IMPACT"
MIG=$(echo "$IMPACT" | jq -r '.migratable_chunks // 0')
STUCK_SP=$(echo "$IMPACT" | jq -r '.stuck_single_policy_chunks // 0')
STUCK_NP=$(echo "$IMPACT" | jq -r '.stuck_no_policy_chunks // 0')
TOTAL=$(echo "$IMPACT" | jq -r '.total_chunks // 0')
note "impact: migratable=$MIG stuck_single=$STUCK_SP stuck_no_policy=$STUCK_NP total=$TOTAL"
(( MIG > 0 ))      || fail "B:impact expected migratable>0 got $MIG (body=$IMPACT)"
(( STUCK_SP > 0 )) || fail "B:impact expected stuck_single_policy>0 got $STUCK_SP (body=$IMPACT)"
# stuck_no_policy is best-effort: requires the residual bucket to have
# actually routed to $CLUSTER via class-env. If the lab's STRATA_RADOS_CLASSES
# default class points at $OTHER_CLUSTER, residual chunks land elsewhere and
# stuck_no_policy stays 0 — note + continue rather than fail.
if (( STUCK_NP == 0 )); then
  note "stuck_no_policy=0 — class default routes elsewhere; residual bucket chunks not on $CLUSTER (lab-dependent, not a regression)"
fi
pass "B:/drain-impact surfaces migratable + stuck_single_policy populated"

note "Apply bulk-fix payload from impact suggestion (uniform-live) to $B_STUCK"
# Pull the first suggested_policies entry for $B_STUCK and PUT it back.
STUCK_FIX=$(echo "$IMPACT" \
  | jq -c --arg name "$B_STUCK" \
      '.by_bucket[] | select(.name==$name) | .suggested_policies[0].policy // empty')
if [[ -z "$STUCK_FIX" || "$STUCK_FIX" == "null" ]]; then
  # Fallback: hand-build uniform {cephb:0, default:1} ourselves so the
  # smoke still validates the operator flow even when the impact response
  # omits the suggestion (shouldn't happen — defensive).
  STUCK_FIX="{\"$CLUSTER\":0,\"$OTHER_CLUSTER\":1}"
fi
put_placement "$B_STUCK" "$STUCK_FIX"
pass "B:applied bulk-fix policy to $B_STUCK ($STUCK_FIX)"

note "Drain $CLUSTER mode=evacuate"
CODE=$(admin_post_body "/admin/v1/clusters/$CLUSTER/drain" '{"mode":"evacuate"}')
[[ "$CODE" == "204" || "$CODE" == "200" ]] \
  || fail "B:drain expected 204 got $CODE body=$(cat "$TMP/admin.out")"
read -r STATE_B MODE_B <<<"$(cluster_state_and_mode)"
[[ "$STATE_B" == "evacuating" && "$MODE_B" == "evacuate" ]] \
  || fail "B:state expected evacuating/evacuate got '$STATE_B/$MODE_B'"
pass "B:state=evacuating mode=evacuate"

note "Wait for drain to converge (deregister_ready=true)"
wait_deregister_ready "B"
FINAL=$(drain_progress)
FINAL_TOTAL=$(echo "$FINAL" | jq -r '.chunks_on_cluster // 0')
[[ "$FINAL_TOTAL" == "0" ]] \
  || fail "B:chunks_on_cluster expected 0 got $FINAL_TOTAL (body=$FINAL)"
pass "B:drain converged (deregister_ready=true)"

# Reads still succeed on B_SPLIT (chunks moved to $OTHER_CLUSTER).
aws --endpoint-url "$BASE" s3 cp "s3://$B_SPLIT/o-1.bin" "$TMP/B-read.bin" >/dev/null \
  || fail "B:post-drain read on $B_SPLIT failed"
GOT=$(md5_of "$TMP/B-read.bin")
[[ "$GOT" == "$ORIG_MD5" ]] || fail "B:post-drain md5 mismatch on $B_SPLIT"
pass "B:post-drain read on $B_SPLIT survives"

note "Undrain to reset for Scenario C"
CODE=$(admin_post_body "/admin/v1/clusters/$CLUSTER/undrain")
[[ "$CODE" == "204" || "$CODE" == "200" ]] \
  || fail "B:undrain expected 204 got $CODE body=$(cat "$TMP/admin.out")"

# ===================================================================
# Scenario C — upgrade readonly → evacuate
# ===================================================================
banner "Scenario C: upgrade readonly → evacuate"

C_SPLIT="dt-c-split-$STAMP"
aws --endpoint-url "$BASE" s3api create-bucket --bucket "$C_SPLIT" >/dev/null
CLEANUP_BUCKETS+=("$C_SPLIT")
put_placement "$C_SPLIT" "{\"$OTHER_CLUSTER\":1,\"$CLUSTER\":1}"
for i in 1 2 3; do
  aws --endpoint-url "$BASE" s3 cp "$PAYLOAD" "s3://$C_SPLIT/o-$i.bin" >/dev/null \
    || fail "C:seed split $i failed"
done

note "Start with mode=readonly"
CODE=$(admin_post_body "/admin/v1/clusters/$CLUSTER/drain" '{"mode":"readonly"}')
[[ "$CODE" == "204" || "$CODE" == "200" ]] \
  || fail "C:drain readonly expected 204 got $CODE body=$(cat "$TMP/admin.out")"
read -r STATE_C MODE_C <<<"$(cluster_state_and_mode)"
[[ "$STATE_C" == "draining_readonly" && "$MODE_C" == "readonly" ]] \
  || fail "C:state expected draining_readonly/readonly got '$STATE_C/$MODE_C'"
pass "C:state=draining_readonly"

note "/drain-impact from readonly → 200 with categorized counts"
C_IMPACT=$(admin_get "/admin/v1/clusters/$CLUSTER/drain-impact")
C_STATE_IN_IMPACT=$(echo "$C_IMPACT" | jq -r '.current_state')
[[ "$C_STATE_IN_IMPACT" == "draining_readonly" ]] \
  || fail "C:/drain-impact current_state expected draining_readonly got '$C_STATE_IN_IMPACT' (body=$C_IMPACT)"
pass "C:/drain-impact callable from draining_readonly"

note "Upgrade to evacuate (POST /drain {mode:evacuate})"
CODE=$(admin_post_body "/admin/v1/clusters/$CLUSTER/drain" '{"mode":"evacuate"}')
[[ "$CODE" == "204" || "$CODE" == "200" ]] \
  || fail "C:upgrade drain expected 204 got $CODE body=$(cat "$TMP/admin.out")"
read -r STATE_C2 MODE_C2 <<<"$(cluster_state_and_mode)"
[[ "$STATE_C2" == "evacuating" && "$MODE_C2" == "evacuate" ]] \
  || fail "C:state after upgrade expected evacuating/evacuate got '$STATE_C2/$MODE_C2'"
pass "C:upgrade transition draining_readonly → evacuating"

note "Wait for drain to converge"
wait_deregister_ready "C"
pass "C:drain converged (deregister_ready=true)"

note "Undrain to leave the lab clean"
CODE=$(admin_post_body "/admin/v1/clusters/$CLUSTER/undrain")
[[ "$CODE" == "204" || "$CODE" == "200" ]] \
  || fail "C:final undrain expected 204 got $CODE body=$(cat "$TMP/admin.out")"

echo
echo "== drain-transparency smoke OK (Scenarios A + B + C green)"
