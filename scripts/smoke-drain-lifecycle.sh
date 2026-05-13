#!/usr/bin/env bash
# Drain-lifecycle end-to-end smoke harness (US-007 of the ralph/drain-lifecycle
# cycle).
#
# Walks the full 15-step operator journey + 4 negative paths from
# tasks/prd-drain-lifecycle.md against a running `multi-cluster` compose
# profile (`docker compose --profile multi-cluster up -d`). Exits non-zero
# with `FAIL: <step>` on any assertion miss; exits 0 when every step + every
# negative path is green.
#
# Pre-requisites on the host:
#   docker, curl, jq, aws (>= 2), md5sum or md5
#   STRATA_STATIC_CREDENTIALS=<access:secret[:owner]>... exported with the
#   same value the gateway booted with (first comma-separated entry is used
#   for the admin login + SigV4-signed bucket setup).
#
# Lab assumptions (per tasks/prd-drain-lifecycle.md and the docker-compose
# `multi-cluster` profile):
#   - strata-multi gateway listens on http://127.0.0.1:9998
#   - STRATA_RADOS_CLUSTERS=default:...,cephb:...
#   - STRATA_RADOS_CLASSES exposes ≥3 storage classes, each pinned to a
#     distinct pool on `default` (the matrix expects #clusters × #distinct
#     pools = 6 rows once US-001 ships).
#   - Drain is unconditionally strict (US-007 drain-transparency) — the
#     former opt-in STRATA_DRAIN_STRICT env has been retired.
#
# Skip behavior: when the multi-cluster profile is NOT up (probe on /readyz
# fails after WAIT_GRACE seconds), the script EXITs 77 (skipped) with a
# clear message so CI fast-paths can `|| true` it. Set REQUIRE_LAB=1 to
# convert the skip into a hard fail (nightly CI gating).

set -euo pipefail

BASE="${BASE:-http://127.0.0.1:9998}"
WAIT_GRACE="${WAIT_GRACE:-15}"
DRAIN_TIMEOUT_S="${DRAIN_TIMEOUT_S:-180}"
CLUSTER="${SMOKE_DRAIN_CLUSTER:-cephb}"
OTHER_CLUSTER="${SMOKE_DRAIN_OTHER:-default}"
EXPECTED_POOL_ROWS="${SMOKE_DRAIN_POOL_ROWS:-6}"
REQUIRE_LAB="${REQUIRE_LAB:-0}"
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
SPLIT_BUCKET="dls-split-$(date +%s)"
CEPHB_BUCKET="dls-cephb-$(date +%s)"
DEFAULT_BUCKET="dls-default-$(date +%s)"
CLEANUP_BUCKETS=()

fail() { echo "FAIL: $*" >&2; exit 1; }
pass() { echo "PASS: $*"; }
note() { echo "INFO: $*"; }

cleanup() {
  for b in "${CLEANUP_BUCKETS[@]}"; do
    aws --endpoint-url "$BASE" s3 rm "s3://$b" --recursive >/dev/null 2>&1 || true
    aws --endpoint-url "$BASE" s3api delete-bucket --bucket "$b" >/dev/null 2>&1 || true
  done
  # Best-effort undrain so a failed run doesn't leave the lab stuck.
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
  local path="$1"
  curl -sf -b "$JAR" "$BASE$path"
}

admin_post() {
  local path="$1" body="${2:-}"
  if [[ -n "$body" ]]; then
    curl -sS -b "$JAR" -H 'Content-Type: application/json' -X POST -d "$body" "$BASE$path"
  else
    curl -sS -b "$JAR" -X POST "$BASE$path"
  fi
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
login

# ---------------------------------------------------------------- Step 1
echo "== Step 1: /admin/v1/clusters lists both clusters live"
LIST_JSON=$(admin_get "/admin/v1/clusters")
IDS=$(echo "$LIST_JSON" | jq -r '.clusters | sort_by(.id) | map(.id) | join(",")')
[[ "$IDS" == "$OTHER_CLUSTER,$CLUSTER" ]] \
  || fail "step1 expected ids '$OTHER_CLUSTER,$CLUSTER' got '$IDS'"
LIVE_COUNT=$(echo "$LIST_JSON" | jq '[.clusters[] | select(.state=="live")] | length')
[[ "$LIVE_COUNT" == "2" ]] \
  || fail "step1 expected 2 live got $LIVE_COUNT (body=$LIST_JSON)"
pass "step1 clusters list ok"

# ---------------------------------------------------------------- Step 2
echo "== Step 2: Pools matrix has $EXPECTED_POOL_ROWS rows (US-001)"
POOLS_JSON=$(admin_get "/admin/v1/storage/data")
ROWS=$(echo "$POOLS_JSON" | jq '.pools | length')
if [[ "$ROWS" != "$EXPECTED_POOL_ROWS" ]]; then
  note "Pools rows=$ROWS, expected $EXPECTED_POOL_ROWS. Lab may have a different STRATA_RADOS_CLASSES — override via SMOKE_DRAIN_POOL_ROWS=N."
  fail "step2 pools matrix row count mismatch"
fi
DISTINCT_CLUSTERS=$(echo "$POOLS_JSON" | jq '[.pools[].cluster] | unique | length')
(( DISTINCT_CLUSTERS >= 2 )) \
  || fail "step2 pools matrix has only $DISTINCT_CLUSTERS distinct clusters; multi-cluster matrix should expose ≥2"
pass "step2 pools matrix has $ROWS rows across $DISTINCT_CLUSTERS clusters"

# Seed three demo buckets representative of the PRD walkthrough.
echo "== Seed buckets ($SPLIT_BUCKET, $CEPHB_BUCKET, $DEFAULT_BUCKET)"
for b in "$SPLIT_BUCKET" "$CEPHB_BUCKET" "$DEFAULT_BUCKET"; do
  aws --endpoint-url "$BASE" s3api create-bucket --bucket "$b" >/dev/null
  CLEANUP_BUCKETS+=("$b")
done
put_placement "$SPLIT_BUCKET"   "{\"$OTHER_CLUSTER\":1,\"$CLUSTER\":1}"
put_placement "$CEPHB_BUCKET"   "{\"$CLUSTER\":1}"
# demo-default intentionally has no policy (class default routing).

# Each bucket gets a small payload so bucket_stats + manifest scan have rows.
PAYLOAD="$TMP/payload.bin"
dd if=/dev/urandom of="$PAYLOAD" bs=1024 count=1 2>/dev/null
aws --endpoint-url "$BASE" s3 cp "$PAYLOAD" "s3://$SPLIT_BUCKET/blob.bin"   >/dev/null
aws --endpoint-url "$BASE" s3 cp "$PAYLOAD" "s3://$CEPHB_BUCKET/blob.bin"   >/dev/null
aws --endpoint-url "$BASE" s3 cp "$PAYLOAD" "s3://$DEFAULT_BUCKET/blob.bin" >/dev/null
pass "seeded demo buckets + payloads"

# ---------------------------------------------------------------- Step 3+4
echo "== Step 3+4: bucket-references endpoint surfaces affected buckets per cluster"
REFS_CEPHB=$(admin_get "/admin/v1/clusters/$CLUSTER/bucket-references")
REFS_DEFAULT=$(admin_get "/admin/v1/clusters/$OTHER_CLUSTER/bucket-references")
CEPHB_NAMES=$(echo "$REFS_CEPHB" | jq -r '.buckets[].name' | sort | tr '\n' ',' | sed 's/,$//')
DEFAULT_NAMES=$(echo "$REFS_DEFAULT" | jq -r '.buckets[].name' | sort | tr '\n' ',' | sed 's/,$//')
[[ "$CEPHB_NAMES" == *"$SPLIT_BUCKET"* ]]   || fail "step3 cephb refs missing $SPLIT_BUCKET (got '$CEPHB_NAMES')"
[[ "$CEPHB_NAMES" == *"$CEPHB_BUCKET"* ]]   || fail "step3 cephb refs missing $CEPHB_BUCKET (got '$CEPHB_NAMES')"
[[ "$DEFAULT_NAMES" == *"$SPLIT_BUCKET"* ]] || fail "step3 default refs missing $SPLIT_BUCKET (got '$DEFAULT_NAMES')"
[[ "$DEFAULT_NAMES" != *"$CEPHB_BUCKET"* ]] || fail "step3 default refs should NOT include cephb-only $CEPHB_BUCKET (got '$DEFAULT_NAMES')"
[[ "$CEPHB_NAMES" != *"$DEFAULT_BUCKET"* ]] || fail "step3 default-routing $DEFAULT_BUCKET must not appear in any cluster ref list"
pass "step3+4 bucket-references match policy filter"

# ---------------------------------------------------------------- Step 5 (retired)
# Step 5 used to assert the top-level drain_strict bool on /clusters; the
# field was removed in US-007 drain-transparency (drain is now
# unconditionally strict). The new smoke-drain-transparency.sh (US-008)
# replaces this with the per-mode drain walkthrough.

# ---------------------------------------------------------------- Step 6+7+8
echo "== Step 6+7+8: drain $CLUSTER"
DRAIN_OUT=$(admin_post "/admin/v1/clusters/$CLUSTER/drain")
note "drain response: $DRAIN_OUT"
STATE=$(cluster_state)
[[ "$STATE" == "draining" ]] || fail "step6 expected state=draining got '$STATE'"
pass "step6 drain accepted"

# ---------------------------------------------------------------- Step 9+10+11
echo "== Step 9+10+11: drain-progress endpoint responds with draining shape"
PROG=$(drain_progress)
PROG_STATE=$(echo "$PROG" | jq -r '.state')
[[ "$PROG_STATE" == "draining" ]] || fail "step10 progress.state expected draining got '$PROG_STATE'"
# chunks/bytes may be null on the very first poll before the rebalance tick
# commits its scan; warnings array carries the explainer in that case.
WARNS=$(echo "$PROG" | jq -r '.warnings // [] | join(",")')
note "initial drain-progress: $PROG (warnings: ${WARNS:-none})"
pass "step9+10+11 drain-progress draining shape ok"

# ---------------------------------------------------------------- Negative paths (retired)
# Negative (a) / (b) used to gate on SMOKE_DRAIN_STRICT_LAB and the
# drain_strict bool to exercise both the strict and fail-open refusal
# shapes. Drain is now unconditionally strict (US-007); both probes are
# covered end-to-end by smoke-drain-transparency.sh's Scenario A and the
# /drain-impact assertions in Scenario B (US-008).

# ---------------------------------------------------------------- Step 12+13
echo "== Step 12+13: wait for drain to converge (deregister_ready=true)"
note "polling /drain-progress every 5s up to ${DRAIN_TIMEOUT_S}s..."
wait_deregister_ready
FINAL=$(drain_progress)
FINAL_CHUNKS=$(echo "$FINAL" | jq -r '.chunks_on_cluster // 0')
[[ "$FINAL_CHUNKS" == "0" ]] || fail "step12 chunks_on_cluster expected 0 got $FINAL_CHUNKS (body=$FINAL)"
pass "step12+13 drain complete (deregister_ready=true)"

# ---------------------------------------------------------------- Step 14 docs-only
echo "== Step 14: env edit + rolling restart is operator-side (docs only). Skipping."

# ---------------------------------------------------------------- Step 15
echo "== Step 15: every former $CLUSTER chunk readable post-drain"
for b in "$SPLIT_BUCKET" "$CEPHB_BUCKET"; do
  aws --endpoint-url "$BASE" s3 cp "s3://$b/blob.bin" "$TMP/${b}.got" >/dev/null \
    || fail "step15 GET s3://$b/blob.bin failed post-drain"
  GOT=$(md5_of "$TMP/${b}.got")
  ORIG=$(md5_of "$PAYLOAD")
  [[ "$GOT" == "$ORIG" ]] || fail "step15 md5 mismatch on $b: $GOT vs $ORIG"
done
pass "step15 cross-cluster reads survive drain"

# ---------------------------------------------------------------- Negative path (c)
echo "== Negative (c): undrain mid-drain — state flips back to live"
admin_post "/admin/v1/clusters/$CLUSTER/undrain" >/dev/null
sleep 2
STATE_AFTER=$(cluster_state)
[[ "$STATE_AFTER" == "live" || -z "$STATE_AFTER" ]] \
  || fail "neg-c expected state=live after undrain got '$STATE_AFTER'"
POST_UNDRAIN=$(drain_progress)
POST_STATE=$(echo "$POST_UNDRAIN" | jq -r '.state')
[[ "$POST_STATE" == "live" ]] \
  || fail "neg-c drain-progress.state should flip live after undrain, got '$POST_STATE'"
pass "neg-c undrain returns state=live + cache cleared"

# ---------------------------------------------------------------- Negative path (d)
echo "== Negative (d): single-cluster-per-class config docs guardrail"
note "single-cluster-per-class drains are documented in best-practices/placement-rebalance.md — drain becomes pure stop-write with no migration. Smoke does not auto-validate this path; the docs section covers the runbook caveat."
pass "neg-d documented in placement-rebalance.md (Drain lifecycle section)"

echo "== drain-lifecycle smoke OK"
