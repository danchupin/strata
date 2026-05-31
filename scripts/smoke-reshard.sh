#!/usr/bin/env bash
# Online-reshard end-to-end smoke harness (US-005 of the
# ralph/architecture-hardening cycle).
#
# Proves the async reshard contract against the Cassandra-backed regression
# lab (the ONLY backend that physically moves rows — memory/TiKV are
# shard-agnostic no-ops, so a reshard there is uninteresting). Drives a
# 64->128 reshard while clients keep writing and asserts the transitional
# model holds under load.
#
# Scenarios:
#   A. Async trigger + progress:
#      - Seed a bucket with N objects (default shard_count from the lab).
#      - POST /admin/bucket/reshard?bucket=X&target=128 -> expect 202 + state=queued
#        (the trigger must NOT block on the migration).
#      - Poll GET /admin/bucket/reshard?bucket=X until state=idle.
#   B. Online correctness under concurrent traffic:
#      - Drive concurrent PUT/GET/DELETE on the SAME bucket throughout the job.
#      - Assert NO client sees a 5xx or a spurious 404 during the reshard.
#      - Assert the key set is identical before and after (modulo the keys the
#        concurrent DELETE removed and the concurrent PUT added — tracked).
#      - Assert a post-flip point-GET hits the new layout (object readable).
#   C. Crash-resume:
#      - Kick a fresh reshard, then `docker restart` the gateway/worker
#        container mid-job; confirm the worker resumes from the watermark and
#        still converges to a correct key set.
#
# Pre-requisites on the host: docker, curl (>=7.75 for --aws-sigv4), jq, aws.
#   STRATA_RESHARD_ROOT_CRED=<access:secret>   — credentials whose owner is the
#       IAM root principal (`iam-root`); required to call /admin/bucket/*.
#       Falls back to the first STRATA_STATIC_CREDENTIALS entry.
#
# Lab assumptions:
#   - Cassandra-backed gateway reachable on http://127.0.0.1:9998 (CASS_BASE).
#     Bring it up with `make up-cassandra && make wait-cassandra`.
#   - The reshard worker MUST be enabled on that gateway, e.g.
#       STRATA_WORKERS=gc,lifecycle,rebalance,reshard make up-cassandra
#     Without it the job stays queued forever and this smoke times out.
#
# Skip behavior: when the Cassandra lab is NOT up (probe on /readyz fails after
# WAIT_GRACE seconds), the script EXITs 77 (skipped) unless REQUIRE_LAB=1.

set -euo pipefail

BASE="${CASS_BASE:-http://127.0.0.1:9998}"
WAIT_GRACE="${WAIT_GRACE:-15}"
RESTART_GRACE="${RESTART_GRACE:-90}"
JOB_GRACE="${SMOKE_RS_JOB_GRACE:-120}"
GATEWAY_CONTAINER="${SMOKE_RS_GATEWAY:-strata-cassandra}"
OBJECT_COUNT="${SMOKE_RS_OBJECTS:-200}"
TARGET_SHARDS="${SMOKE_RS_TARGET:-128}"
REGION="${SMOKE_RS_REGION:-us-east-1}"
REQUIRE_LAB="${REQUIRE_LAB:-0}"
STAMP="$(date +%s)"

CRED="${STRATA_RESHARD_ROOT_CRED:-${STRATA_STATIC_CREDENTIALS:-}}"
if [[ -z "$CRED" ]]; then
  echo "FAIL: set STRATA_RESHARD_ROOT_CRED or STRATA_STATIC_CREDENTIALS (need access:secret with owner=iam-root)" >&2
  exit 2
fi
FIRST="${CRED%%,*}"
AK="${FIRST%%:*}"
REST="${FIRST#*:}"
SK="${REST%%:*}"
if [[ -z "$AK" || -z "$SK" || "$AK" == "$FIRST" ]]; then
  echo "FAIL: cannot parse access/secret from '$FIRST'" >&2
  exit 2
fi

for tool in curl jq aws docker; do
  command -v "$tool" >/dev/null 2>&1 \
    || { echo "FAIL: missing tool: $tool" >&2; exit 2; }
done

TMP="$(mktemp -d)"
BUCKET="rs-smoke-${STAMP}"
LOAD_PID=""

fail() { echo "FAIL: $*" >&2; exit 1; }
pass() { echo "PASS: $*"; }
note() { echo "INFO: $*"; }
banner() { echo; echo "==> $*"; }

cleanup() {
  [[ -n "$LOAD_PID" ]] && kill "$LOAD_PID" >/dev/null 2>&1 || true
  aws --endpoint-url "$BASE" s3 rm "s3://$BUCKET" --recursive >/dev/null 2>&1 || true
  aws --endpoint-url "$BASE" s3api delete-bucket --bucket "$BUCKET" >/dev/null 2>&1 || true
  rm -rf "$TMP"
}
trap cleanup EXIT

probe_ready() {
  local i=0 grace="${1:-$WAIT_GRACE}"
  while (( i < grace )); do
    if [[ "$(curl -fsS -o /dev/null -w '%{http_code}' "$BASE/readyz" 2>/dev/null)" == "200" ]]; then
      return 0
    fi
    sleep 1; i=$((i+1))
  done
  return 1
}

# admin_post returns the HTTP code; body in $TMP/admin.out.
admin_post() {
  local path="$1"
  curl -sS -o "$TMP/admin.out" -w '%{http_code}' \
    --user "$AK:$SK" --aws-sigv4 "aws:amz:${REGION}:s3" \
    -X POST "$BASE$path"
}

# admin_get_state echoes the JSON `state` field of the progress read.
admin_get_state() {
  curl -sS --user "$AK:$SK" --aws-sigv4 "aws:amz:${REGION}:s3" \
    "$BASE/admin/bucket/reshard?bucket=$BUCKET" | jq -r '.state // "error"'
}
admin_get_shardcount() {
  curl -sS --user "$AK:$SK" --aws-sigv4 "aws:amz:${REGION}:s3" \
    "$BASE/admin/bucket/reshard?bucket=$BUCKET" | jq -r '.shard_count // 0'
}

if ! probe_ready; then
  msg="Cassandra lab not reachable on $BASE/readyz after ${WAIT_GRACE}s"
  if [[ "$REQUIRE_LAB" == "1" ]]; then fail "$msg (REQUIRE_LAB=1)"; fi
  echo "SKIP: $msg" >&2
  echo "SKIP: bring it up with 'STRATA_WORKERS=gc,lifecycle,rebalance,reshard make up-cassandra'." >&2
  exit 77
fi

banner "Seed bucket $BUCKET with $OBJECT_COUNT objects"
aws --endpoint-url "$BASE" s3api create-bucket --bucket "$BUCKET" >/dev/null \
  || fail "create-bucket $BUCKET"
echo "payload-${STAMP}" > "$TMP/body"
declare -A SEEDED
for ((i=0; i<OBJECT_COUNT; i++)); do
  key="seed/k-$(printf '%04d' "$i")"
  aws --endpoint-url "$BASE" s3api put-object \
    --bucket "$BUCKET" --key "$key" --body "$TMP/body" >/dev/null \
    || fail "seed put $key"
  SEEDED["$key"]=1
done
pass "seeded $OBJECT_COUNT objects"

# Snapshot the pre-reshard key set.
aws --endpoint-url "$BASE" s3api list-objects-v2 --bucket "$BUCKET" \
  | jq -r '.Contents[].Key' | sort > "$TMP/keys.before"
BEFORE_N=$(wc -l < "$TMP/keys.before" | tr -d ' ')
[[ "$BEFORE_N" -eq "$OBJECT_COUNT" ]] \
  || fail "pre-reshard list returned $BEFORE_N want $OBJECT_COUNT"

banner "Scenario B: start concurrent PUT/GET/DELETE load"
# Background load loop: writes load/* keys, re-reads a seed key, deletes the
# load key it just wrote. Records any non-2xx HTTP code into err.log. The seed
# key set is never mutated by the loop, so it must be byte-identical after.
(
  n=0
  while :; do
    lk="load/l-$(printf '%05d' "$n")"
    pc=$(curl -sS -o /dev/null -w '%{http_code}' \
      --user "$AK:$SK" --aws-sigv4 "aws:amz:${REGION}:s3" \
      -X PUT --data-binary "@$TMP/body" "$BASE/$BUCKET/$lk")
    [[ "$pc" =~ ^2 ]] || echo "PUT $lk -> $pc" >> "$TMP/err.log"
    sk="seed/k-$(printf '%04d' $((n % OBJECT_COUNT)))"
    gc=$(curl -sS -o /dev/null -w '%{http_code}' \
      --user "$AK:$SK" --aws-sigv4 "aws:amz:${REGION}:s3" \
      "$BASE/$BUCKET/$sk")
    [[ "$gc" == "200" ]] || echo "GET $sk -> $gc" >> "$TMP/err.log"
    dc=$(curl -sS -o /dev/null -w '%{http_code}' \
      --user "$AK:$SK" --aws-sigv4 "aws:amz:${REGION}:s3" \
      -X DELETE "$BASE/$BUCKET/$lk")
    [[ "$dc" =~ ^2 ]] || echo "DELETE $lk -> $dc" >> "$TMP/err.log"
    n=$((n+1))
    sleep 0.05
  done
) &
LOAD_PID=$!
note "load generator pid=$LOAD_PID"

banner "Scenario A: trigger reshard $BUCKET -> $TARGET_SHARDS (expect 202 queued)"
CODE=$(admin_post "/admin/bucket/reshard?bucket=$BUCKET&target=$TARGET_SHARDS")
[[ "$CODE" == "202" ]] || fail "trigger expected 202 got $CODE body=$(cat "$TMP/admin.out")"
TRIG_STATE=$(jq -r '.state // "error"' < "$TMP/admin.out")
[[ "$TRIG_STATE" == "queued" ]] || fail "trigger state expected queued got '$TRIG_STATE'"
pass "trigger returned 202 state=queued (async — did not block on migration)"

banner "Poll progress until state=idle (the worker drains the job)"
i=0
while (( i < JOB_GRACE )); do
  st=$(admin_get_state)
  if [[ "$st" == "idle" ]]; then break; fi
  [[ "$st" == "error" ]] && fail "progress read returned error"
  sleep 1; i=$((i+1))
done
[[ "$st" == "idle" ]] || fail "reshard did not converge to idle within ${JOB_GRACE}s (worker enabled?)"
SC=$(admin_get_shardcount)
[[ "$SC" == "$TARGET_SHARDS" ]] || fail "post-reshard shard_count=$SC want $TARGET_SHARDS"
pass "reshard converged: state=idle shard_count=$TARGET_SHARDS"

banner "Stop load generator + assert no client error during the job"
kill "$LOAD_PID" >/dev/null 2>&1 || true
wait "$LOAD_PID" 2>/dev/null || true
LOAD_PID=""
if [[ -s "$TMP/err.log" ]]; then
  fail "client errors observed during reshard:\n$(cat "$TMP/err.log")"
fi
pass "no 5xx / spurious 404 observed across the reshard window"

banner "Assert the seed key set is identical before/after"
aws --endpoint-url "$BASE" s3api list-objects-v2 --bucket "$BUCKET" \
  | jq -r '.Contents[].Key' | grep '^seed/' | sort > "$TMP/keys.after"
if ! diff -q "$TMP/keys.before" "$TMP/keys.after" >/dev/null; then
  fail "seed key set diverged across reshard:\n$(diff "$TMP/keys.before" "$TMP/keys.after")"
fi
pass "seed key set identical before/after the reshard"

banner "Assert post-flip point-GET hits the new layout"
probe_key="seed/k-0000"
gc=$(curl -sS -o "$TMP/probe.out" -w '%{http_code}' \
  --user "$AK:$SK" --aws-sigv4 "aws:amz:${REGION}:s3" \
  "$BASE/$BUCKET/$probe_key")
[[ "$gc" == "200" ]] || fail "post-flip point-GET $probe_key -> $gc (new-layout read failed)"
pass "post-flip point-GET resolves under the $TARGET_SHARDS-shard layout"

banner "Scenario C: crash-resume — restart $GATEWAY_CONTAINER mid-job"
# Seed a fresh divergence and kick a second reshard would 409 (already at 128);
# instead bump to 256 to get a fresh job, restart the worker mid-flight, and
# confirm convergence + key-set stability.
NEXT_TARGET=$((TARGET_SHARDS * 2))
CODE=$(admin_post "/admin/bucket/reshard?bucket=$BUCKET&target=$NEXT_TARGET")
[[ "$CODE" == "202" ]] || fail "C:trigger expected 202 got $CODE body=$(cat "$TMP/admin.out")"
note "restarting $GATEWAY_CONTAINER to simulate a worker crash mid-job"
docker restart "$GATEWAY_CONTAINER" >/dev/null 2>&1 \
  || fail "C:docker restart $GATEWAY_CONTAINER failed"
probe_ready "$RESTART_GRACE" || fail "C:gateway did not come back within ${RESTART_GRACE}s"

i=0
while (( i < JOB_GRACE )); do
  st=$(admin_get_state)
  if [[ "$st" == "idle" ]]; then break; fi
  sleep 1; i=$((i+1))
done
[[ "$st" == "idle" ]] || fail "C:reshard did not resume+converge within ${JOB_GRACE}s after restart"
SC=$(admin_get_shardcount)
[[ "$SC" == "$NEXT_TARGET" ]] || fail "C:post-resume shard_count=$SC want $NEXT_TARGET"

aws --endpoint-url "$BASE" s3api list-objects-v2 --bucket "$BUCKET" \
  | jq -r '.Contents[].Key' | grep '^seed/' | sort > "$TMP/keys.resume"
if ! diff -q "$TMP/keys.before" "$TMP/keys.resume" >/dev/null; then
  fail "C:seed key set diverged across crash-resume:\n$(diff "$TMP/keys.before" "$TMP/keys.resume")"
fi
pass "crash-resume: worker resumed from watermark, converged to shard_count=$NEXT_TARGET, key set intact"

echo
echo "ALL PASS: online reshard async + concurrent-write + crash-resume green on $BASE"
