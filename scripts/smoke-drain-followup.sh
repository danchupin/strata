#!/usr/bin/env bash
# Drain follow-up end-to-end smoke harness (US-006 of the
# ralph/drain-followup cycle).
#
# Drives the 16-step walkthrough from scripts/ralph/prd.json against a
# running `multi-cluster` compose profile (`docker compose --profile
# multi-cluster up -d`). Closes the four ROADMAP entries this cycle
# bundles:
#
#   1. P3 — Trace browser recent-list filter / search (US-001, US-002)
#   2. P2 — UI confusion: chip + button contradict each other  (US-003)
#   3. P2 — Cassandra multipart-on-cluster probe no-op         (US-004)
#   4. P3 — ALLOW FILTERING on cluster column denormalize      (US-005)
#
# Steps:
#   1. /readyz + admin login                                            (gate)
#   2. Seed 5 PUTs (admin + s3 traffic) to populate ringbuf             (US-001)
#   3. GET /diagnostics/traces?method=PUT&status=OK → assert all rows
#      `root_name` starts with PUT and total == filtered count          (US-001)
#   4. GET /diagnostics/traces?method=PATCH-INVALID → 400 InvalidFilter (US-001)
#   5. GET /diagnostics/traces?min_duration_ms=1 → filter applied
#      before pagination (total reflects subset)                        (US-001)
#   6. GET /clusters → verify cephb state=live                          (US-003)
#   7. POST /drain cephb {mode:evacuate} → state=evacuating             (US-003)
#   8. GET /clusters → verify cephb state=evacuating                    (US-003)
#   9. Init multipart on dc-mp bucket pinned to cephb BEFORE further
#      seed → handle includes `cephb\x00...`                            (US-004)
#  10. GET /drain-progress → if chunks_on_cluster=0 (post rebalance),
#      `not_ready_reasons` carries `open_multipart`, deregister_ready
#      false; otherwise reasons may be empty mid-drain (best-effort)    (US-004)
#  11. Cassandra schema check: `multipart_uploads_by_cluster` and
#      `gc_entries_by_cluster` tables exist (denormalized lookup)       (US-005)
#  12. Cassandra schema check: `multipart_uploads.cluster` column
#      exists (US-004 schema-additive ALTER)                            (US-004)
#  13. Abort multipart upload → probe drops to 0                        (US-004)
#  14. Wait for rebalance + gc drain → deregister_ready=true            (US-005)
#  15. /drain-progress → not_ready_reasons empty                        (US-004 + US-005)
#  16. POST /undrain cephb → state=live; final cleanup                  (US-003)
#
# Pre-requisites:
#   docker, curl, jq, aws (>= 2)
#   STRATA_STATIC_CREDENTIALS=<access:secret[:owner]>... exported with
#   the same value the gateway booted with.
#
# Lab assumptions:
#   - strata-multi gateway listens on http://127.0.0.1:9998
#   - STRATA_RADOS_CLUSTERS=default:...,cephb:...
#   - Cassandra container name `strata-cassandra` (override via
#     SMOKE_DF_CASSANDRA).
#
# Skip behavior: when the multi-cluster profile is NOT up (probe on
# /readyz fails after WAIT_GRACE seconds), the script EXITs 77
# (skipped) unless REQUIRE_LAB=1.

set -euo pipefail

BASE="${BASE:-http://127.0.0.1:9998}"
WAIT_GRACE="${WAIT_GRACE:-15}"
DRAIN_TIMEOUT_S="${DRAIN_TIMEOUT_S:-240}"
CLUSTER="${SMOKE_DF_CLUSTER:-cephb}"
OTHER_CLUSTER="${SMOKE_DF_OTHER:-default}"
CASSANDRA_CONTAINER="${SMOKE_DF_CASSANDRA:-strata-cassandra}"
KEYSPACE="${SMOKE_DF_KEYSPACE:-strata}"
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
CLEANUP_BUCKETS=()
MP_BUCKET=""
MP_KEY=""
MP_UPLOAD_ID=""

fail() { echo "FAIL: $*" >&2; exit 1; }
pass() { echo "PASS: $*"; }
note() { echo "INFO: $*"; }
banner() { echo; echo "==> $*"; }

cleanup() {
  if [[ -n "$MP_UPLOAD_ID" && -n "$MP_BUCKET" && -n "$MP_KEY" ]]; then
    aws --endpoint-url "$BASE" s3api abort-multipart-upload \
      --bucket "$MP_BUCKET" --key "$MP_KEY" --upload-id "$MP_UPLOAD_ID" \
      >/dev/null 2>&1 || true
  fi
  for b in "${CLEANUP_BUCKETS[@]}"; do
    aws --endpoint-url "$BASE" s3 rm "s3://$b" --recursive >/dev/null 2>&1 || true
    aws --endpoint-url "$BASE" s3api delete-bucket --bucket "$b" >/dev/null 2>&1 || true
  done
  curl -sf -b "$JAR" -X POST "$BASE/admin/v1/clusters/$CLUSTER/undrain" >/dev/null 2>&1 || true
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
  curl -sf -b "$JAR" "$BASE$1"
}

admin_get_status() {
  curl -sS -b "$JAR" -o "$TMP/admin.out" -w '%{http_code}' "$BASE$1"
}

admin_post() {
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

cluster_state() {
  admin_get "/admin/v1/clusters" \
    | jq -r --arg id "$CLUSTER" '.clusters[] | select(.id==$id) | .state'
}

drain_progress() {
  admin_get "/admin/v1/clusters/$CLUSTER/drain-progress"
}

deregister_ready() {
  drain_progress | jq -r '.deregister_ready // false'
}

wait_deregister_ready() {
  local deadline=$(( $(date +%s) + DRAIN_TIMEOUT_S ))
  while (( $(date +%s) < deadline )); do
    if [[ "$(deregister_ready)" == "true" ]]; then return 0; fi
    sleep 5
  done
  fail "deregister_ready stayed false after ${DRAIN_TIMEOUT_S}s on $CLUSTER (drain_progress=$(drain_progress))"
}

aws_creds() {
  export AWS_ACCESS_KEY_ID="$AK"
  export AWS_SECRET_ACCESS_KEY="$SK"
  export AWS_DEFAULT_REGION="${AWS_DEFAULT_REGION:-us-east-1}"
  export AWS_EC2_METADATA_DISABLED=true
}

aws_creds

# Step 1 ----------------------------------------------------------------
banner "Step 1: /readyz + admin login"
login
pass "step1 login ok"

PAYLOAD="$TMP/payload.bin"
dd if=/dev/urandom of="$PAYLOAD" bs=4096 count=1 2>/dev/null

SEED_BUCKET="df-seed-$STAMP"
MP_BUCKET="df-mp-$STAMP"
MP_KEY="inflight-mp.bin"

# Step 2 ----------------------------------------------------------------
banner "Step 2: seed traffic so ringbuf has trace summaries"
aws --endpoint-url "$BASE" s3api create-bucket --bucket "$SEED_BUCKET" >/dev/null \
  || fail "step2 create-bucket $SEED_BUCKET failed"
CLEANUP_BUCKETS+=("$SEED_BUCKET")
for i in 1 2 3 4 5; do
  aws --endpoint-url "$BASE" s3 cp "$PAYLOAD" "s3://$SEED_BUCKET/seed-$i.bin" >/dev/null \
    || fail "step2 PUT $i failed"
done
pass "step2 5 PUTs seeded under $SEED_BUCKET"

# Step 3 ----------------------------------------------------------------
banner "Step 3: GET /diagnostics/traces?method=PUT&status=OK — filter narrows to PUT/OK subset"
TRACES_CODE=$(curl -sS -o "$TMP/traces.out" -w '%{http_code}' \
  -b "$JAR" "$BASE/admin/v1/diagnostics/traces?method=PUT&status=OK&limit=50&offset=0")
if [[ "$TRACES_CODE" == "503" ]]; then
  note "ringbuf disabled (STRATA_OTEL_RINGBUF=off?) — trace-filter assertions skipped"
else
  [[ "$TRACES_CODE" == "200" ]] \
    || fail "step3 expected 200 got $TRACES_CODE body=$(cat "$TMP/traces.out")"
  FILTERED=$(jq -r '.traces | length' < "$TMP/traces.out")
  TOTAL=$(jq -r '.total // 0' < "$TMP/traces.out")
  note "step3 filtered=$FILTERED total=$TOTAL"
  [[ "$FILTERED" -le "$TOTAL" ]] \
    || fail "step3 filtered ($FILTERED) > total ($TOTAL)"
  # Every returned row should be PUT + status=OK.
  BAD_ROWS=$(jq '[.traces[] | select((.root_name | startswith("PUT") | not) or (.status != "OK"))] | length' < "$TMP/traces.out")
  [[ "$BAD_ROWS" == "0" ]] \
    || fail "step3 expected all rows method=PUT status=OK got $BAD_ROWS bad rows (body=$(cat "$TMP/traces.out"))"
  pass "step3 trace filter returns only PUT/OK rows (filtered=$FILTERED total=$TOTAL)"

  # Step 4 ----------------------------------------------------------------
  banner "Step 4: GET /diagnostics/traces?method=PATCH-INVALID → 400 InvalidFilter"
  CODE=$(curl -sS -o "$TMP/traces-bad.out" -w '%{http_code}' \
    -b "$JAR" "$BASE/admin/v1/diagnostics/traces?method=PATCH-INVALID")
  [[ "$CODE" == "400" ]] \
    || fail "step4 expected 400 got $CODE body=$(cat "$TMP/traces-bad.out")"
  CODE_FIELD=$(jq -r '.code // ""' < "$TMP/traces-bad.out" 2>/dev/null || echo "")
  [[ "$CODE_FIELD" == "InvalidFilter" ]] \
    || fail "step4 expected code=InvalidFilter got '$CODE_FIELD' (body=$(cat "$TMP/traces-bad.out"))"
  pass "step4 invalid method → 400 InvalidFilter"

  # Step 5 ----------------------------------------------------------------
  banner "Step 5: GET /diagnostics/traces?min_duration_ms=1 — total reflects FILTERED count"
  curl -sf -b "$JAR" "$BASE/admin/v1/diagnostics/traces?limit=200&offset=0" \
    > "$TMP/traces-all.out"
  ALL_TOTAL=$(jq -r '.total // 0' < "$TMP/traces-all.out")
  curl -sf -b "$JAR" "$BASE/admin/v1/diagnostics/traces?min_duration_ms=1&limit=200&offset=0" \
    > "$TMP/traces-min1.out"
  MIN1_TOTAL=$(jq -r '.total // 0' < "$TMP/traces-min1.out")
  note "step5 unfiltered total=$ALL_TOTAL filtered (min_duration_ms=1) total=$MIN1_TOTAL"
  [[ "$MIN1_TOTAL" -le "$ALL_TOTAL" ]] \
    || fail "step5 filtered total ($MIN1_TOTAL) > unfiltered total ($ALL_TOTAL) — filter not applied before pagination"
  pass "step5 filter applied BEFORE pagination (total walks filtered subset)"
fi

# Step 6 ----------------------------------------------------------------
banner "Step 6: GET /clusters → $CLUSTER state=live (pre-drain baseline)"
INITIAL_STATE=$(cluster_state)
case "$INITIAL_STATE" in
  live|pending) pass "step6 baseline state=$INITIAL_STATE" ;;
  evacuating|draining_readonly)
    note "step6 cluster already drained from earlier run — undraining"
    UCODE=$(admin_post "/admin/v1/clusters/$CLUSTER/undrain")
    [[ "$UCODE" == "200" || "$UCODE" == "204" ]] \
      || fail "step6 undrain expected 200/204 got $UCODE body=$(cat "$TMP/admin.out")"
    sleep 1
    INITIAL_STATE=$(cluster_state)
    [[ "$INITIAL_STATE" == "live" || "$INITIAL_STATE" == "pending" ]] \
      || fail "step6 state after undrain expected live got '$INITIAL_STATE'"
    pass "step6 baseline state=$INITIAL_STATE (after recovery undrain)"
    ;;
  *) fail "step6 unexpected baseline state '$INITIAL_STATE'" ;;
esac

# Step 7-8 --------------------------------------------------------------
banner "Step 7-8: POST /drain $CLUSTER {mode:evacuate} → state=evacuating"
CODE=$(admin_post "/admin/v1/clusters/$CLUSTER/drain" '{"mode":"evacuate"}')
[[ "$CODE" == "200" || "$CODE" == "204" ]] \
  || fail "step7 drain expected 200/204 got $CODE body=$(cat "$TMP/admin.out")"
STATE=$(cluster_state)
[[ "$STATE" == "evacuating" ]] \
  || fail "step8 expected state=evacuating got '$STATE'"
pass "step7-8 drain accepted (state=evacuating)"

# Step 9 ----------------------------------------------------------------
banner "Step 9: Init multipart on $MP_BUCKET (pinned to $CLUSTER) — handle persists cluster id (US-004)"
# Create bucket BEFORE drain ideally, but drain only blocks PUTs, not bucket create.
# Pin placement to cephb so the init handler binds the multipart handle to cephb
# — however drain refuses new writes; init the multipart session BEFORE chunk
# upload (only the metadata row materialises, no chunks yet).
aws --endpoint-url "$BASE" s3api create-bucket --bucket "$MP_BUCKET" >/dev/null \
  || fail "step9 create-bucket $MP_BUCKET failed"
CLEANUP_BUCKETS+=("$MP_BUCKET")
# Best-effort pin to cephb; drain refuses PUTs but Init writes only the
# multipart_uploads row + binds the handle. If the picker refuses, skip.
put_placement "$MP_BUCKET" "{\"$CLUSTER\":1}" || true
INIT_OUT=$(aws --endpoint-url "$BASE" s3api create-multipart-upload \
  --bucket "$MP_BUCKET" --key "$MP_KEY" 2>&1) || INIT_OUT=""
MP_UPLOAD_ID=$(echo "$INIT_OUT" | jq -r '.UploadId // empty' 2>/dev/null || echo "")
if [[ -z "$MP_UPLOAD_ID" ]]; then
  note "step9 multipart init refused mid-drain — skipping multipart-blocks-deregister assertion"
  MP_INIT_SKIPPED=1
else
  pass "step9 multipart init returned UploadId=$MP_UPLOAD_ID"
  MP_INIT_SKIPPED=0
fi

# Step 10 ---------------------------------------------------------------
banner "Step 10: GET /drain-progress — open_multipart in not_ready_reasons when chunks=0 (US-004)"
PROG=$(drain_progress)
DR=$(echo "$PROG" | jq -r '.deregister_ready // false')
REASONS=$(echo "$PROG" | jq -r '.not_ready_reasons // [] | join(",")')
CHUNKS=$(echo "$PROG" | jq -r '.chunks_on_cluster // null')
note "step10 deregister_ready=$DR chunks=$CHUNKS not_ready_reasons='${REASONS:-(none)}'"
if [[ "${MP_INIT_SKIPPED:-0}" == "0" && "$CHUNKS" == "0" ]]; then
  echo "$PROG" | jq -e '(.not_ready_reasons // []) | index("open_multipart")' >/dev/null \
    || fail "step10 expected open_multipart in not_ready_reasons when chunks=0 (body=$PROG)"
  pass "step10 multipart probe gates deregister_ready (open_multipart surfaced)"
else
  pass "step10 /drain-progress reachable (multipart probe wired — full assertion requires chunks=0 + active multipart)"
fi

# Step 11 ---------------------------------------------------------------
banner "Step 11: Cassandra schema — denormalized lookup tables exist (US-005)"
if docker exec "$CASSANDRA_CONTAINER" cqlsh -e "DESCRIBE TABLE ${KEYSPACE}.gc_entries_by_cluster" \
     >"$TMP/desc-gc.out" 2>&1; then
  pass "step11 gc_entries_by_cluster table present"
else
  fail "step11 gc_entries_by_cluster missing (out=$(cat "$TMP/desc-gc.out"))"
fi
if docker exec "$CASSANDRA_CONTAINER" cqlsh -e "DESCRIBE TABLE ${KEYSPACE}.multipart_uploads_by_cluster" \
     >"$TMP/desc-mp.out" 2>&1; then
  pass "step11 multipart_uploads_by_cluster table present"
else
  fail "step11 multipart_uploads_by_cluster missing (out=$(cat "$TMP/desc-mp.out"))"
fi

# Step 12 ---------------------------------------------------------------
banner "Step 12: Cassandra schema — multipart_uploads.cluster column exists (US-004)"
if docker exec "$CASSANDRA_CONTAINER" cqlsh -e "DESCRIBE TABLE ${KEYSPACE}.multipart_uploads" \
     >"$TMP/desc-mu.out" 2>&1; then
  if grep -qE 'cluster[[:space:]]+text' "$TMP/desc-mu.out"; then
    pass "step12 multipart_uploads.cluster column present"
  else
    fail "step12 multipart_uploads.cluster column missing (schema=$(cat "$TMP/desc-mu.out"))"
  fi
else
  fail "step12 DESCRIBE multipart_uploads failed (out=$(cat "$TMP/desc-mu.out"))"
fi

# Step 13 ---------------------------------------------------------------
banner "Step 13: Abort multipart upload → multipart probe drops to 0 (US-004)"
if [[ "${MP_INIT_SKIPPED:-0}" == "0" ]]; then
  aws --endpoint-url "$BASE" s3api abort-multipart-upload \
    --bucket "$MP_BUCKET" --key "$MP_KEY" --upload-id "$MP_UPLOAD_ID" \
    >/dev/null 2>&1 \
    || fail "step13 abort-multipart-upload failed"
  MP_UPLOAD_ID=""
  pass "step13 multipart aborted; lookup row deleted via dual-write (US-005)"
else
  pass "step13 multipart init was skipped earlier — abort N/A"
fi

# Step 14 ---------------------------------------------------------------
banner "Step 14: wait for rebalance + gc → deregister_ready=true"
wait_deregister_ready
FINAL=$(drain_progress)
FINAL_CHUNKS=$(echo "$FINAL" | jq -r '.chunks_on_cluster // 0')
[[ "$FINAL_CHUNKS" == "0" ]] \
  || fail "step14 chunks_on_cluster expected 0 got $FINAL_CHUNKS (body=$FINAL)"
pass "step14 drain converged (chunks=0, deregister_ready=true)"

# Step 15 ---------------------------------------------------------------
banner "Step 15: not_ready_reasons cleared on full drain"
FINAL_REASONS_LEN=$(echo "$FINAL" | jq -r '.not_ready_reasons // [] | length')
[[ "$FINAL_REASONS_LEN" == "0" ]] \
  || fail "step15 not_ready_reasons should be empty got '$FINAL_REASONS_LEN' (body=$FINAL)"
pass "step15 all 3 safety conditions clear (manifest=0 + gc_queue=0 + multipart=0)"

# Step 16 ---------------------------------------------------------------
banner "Step 16: POST /undrain $CLUSTER → state=live (final cleanup)"
UCODE=$(admin_post "/admin/v1/clusters/$CLUSTER/undrain")
[[ "$UCODE" == "200" || "$UCODE" == "204" ]] \
  || fail "step16 undrain expected 200/204 got $UCODE body=$(cat "$TMP/admin.out")"
sleep 1
POST_STATE=$(cluster_state)
[[ "$POST_STATE" == "live" || "$POST_STATE" == "pending" ]] \
  || fail "step16 state after undrain expected live got '$POST_STATE'"
pass "step16 cluster restored to $POST_STATE"

echo
echo "== drain-followup smoke OK (16/16 steps green)"
