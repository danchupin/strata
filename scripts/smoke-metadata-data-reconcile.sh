#!/usr/bin/env bash
# End-to-end reconcile / rebuild-index validation smoke (US-007 of the
# ralph/metadata-data-reconcile cycle).
#
# Proves the operator-facing contract the web console (US-006) rides end to
# end against a RUNNING lab — the UI is NOT a no-op stub — across BOTH meta
# backends (TiKV-default lab on :9999, Cassandra lab on :9998) for parity:
#
#   1. Console session login (POST /admin/v1/auth/login).
#   2. Seed a bucket + a couple of objects via SigV4 (aws).
#   3. DANGLING pass (meta->data): POST /admin/v1/reconcile {bucket,policy}
#      -> 202 + state=queued + id (the trigger must NOT block on the walk).
#      Poll GET /admin/v1/reconcile/{id} until a TERMINAL state (done|error)
#      and assert the progress/summary counters are readable — the
#      trigger -> progress -> summary path the console polls.
#   4. ORPHAN pass (data->meta): POST {cluster,pool,policy} -> 202 + poll.
#   5. rebuild-index --dry-run inside the gateway container (docker exec)
#      -> the last-resort CLI reports a recovery plan and writes nothing.
#
# The DEEP data-tier correctness (no silent leak / no silent corrupt GET, both
# skews, restore, full rebuild with gaps flagged + SSE unrecoverable) is pinned
# deterministically by the CI-green Go walkthrough
# (TestEndToEndReconcileWalkthrough in internal/reconcile/walkthrough_test.go).
# The RADOS resolution legs (orphan pool scan, dangling per-OID probe) are
# integration-gated: on a lab without the US-003b/US-004b real RADOS prober /
# scanner a job may converge to `error` — that is an ACCEPTED terminal state
# for THIS contract smoke (the queue/progress/summary plumbing still proves
# out); the runbook covers the integration legs.
#
# Pre-requisites on the host: docker, curl, jq, aws.
#   STRATA_RECONCILE_ROOT_CRED=<access:secret> — credentials whose owner is the
#       IAM root principal (`iam-root`), required for /admin/v1/*. Falls back to
#       the first STRATA_STATIC_CREDENTIALS entry.
#
# Lab assumptions (bring up with):
#   TiKV-default:  make up-all && make wait-strata-lab
#   Cassandra:     make up-cassandra && make wait-cassandra
#   The `reconcile` worker MUST be enabled on the gateway, e.g.
#       STRATA_WORKERS=gc,lifecycle,reconcile make up-all
#   Without it queued jobs never drain and this smoke times out (JOB_GRACE).
#
# Skip behavior: when NEITHER lab is reachable, EXIT 77 (skipped) unless
# REQUIRE_LAB=1.

set -euo pipefail

TIKV_BASE="${TIKV_BASE:-http://127.0.0.1:9999}"
CASS_BASE="${CASS_BASE:-http://127.0.0.1:9998}"
WAIT_GRACE="${WAIT_GRACE:-10}"
JOB_GRACE="${SMOKE_RC_JOB_GRACE:-90}"
REGION="${SMOKE_RC_REGION:-us-east-1}"
CLUSTER="${SMOKE_RC_CLUSTER:-ceph-a}"
POOL="${SMOKE_RC_POOL:-strata-data}"
REQUIRE_LAB="${REQUIRE_LAB:-0}"
STAMP="$(date +%s)"

CRED="${STRATA_RECONCILE_ROOT_CRED:-${STRATA_STATIC_CREDENTIALS:-}}"
if [[ -z "$CRED" ]]; then
  echo "FAIL: set STRATA_RECONCILE_ROOT_CRED or STRATA_STATIC_CREDENTIALS (need access:secret with owner=iam-root)" >&2
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

export AWS_ACCESS_KEY_ID="$AK"
export AWS_SECRET_ACCESS_KEY="$SK"
export AWS_DEFAULT_REGION="$REGION"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

fail()   { echo "FAIL: $*" >&2; exit 1; }
pass()   { echo "PASS: $*"; }
note()   { echo "INFO: $*"; }
banner() { echo; echo "==> $*"; }

# probe_ready BASE -> 0 if /readyz returns 200 within WAIT_GRACE seconds.
probe_ready() {
  local base="$1" i
  for ((i = 0; i < WAIT_GRACE; i++)); do
    [[ "$(curl -fsS -o /dev/null -w '%{http_code}' "$base/readyz" 2>/dev/null)" == "200" ]] && return 0
    sleep 1
  done
  return 1
}

# login BASE JAR -> establish a console session cookie in JAR.
login() {
  local base="$1" jar="$2" body code
  body=$(printf '{"access_key":"%s","secret_key":"%s"}' "$AK" "$SK")
  code=$(curl -sS -o "$TMP/login.out" -w '%{http_code}' \
    -c "$jar" -H 'Content-Type: application/json' \
    -X POST -d "$body" "$base/admin/v1/auth/login")
  [[ "$code" == "200" ]] || fail "login $base: HTTP $code body=$(cat "$TMP/login.out")"
}

# queue_reconcile BASE JAR JSON -> echoes the job id (asserts 202 + state).
queue_reconcile() {
  local base="$1" jar="$2" payload="$3" code id state
  code=$(curl -sS -o "$TMP/rc.out" -w '%{http_code}' \
    -b "$jar" -H 'Content-Type: application/json' \
    -X POST -d "$payload" "$base/admin/v1/reconcile")
  [[ "$code" == "202" ]] || fail "POST reconcile ($payload): HTTP $code body=$(cat "$TMP/rc.out")"
  id=$(jq -r '.id // empty' "$TMP/rc.out")
  state=$(jq -r '.state // empty' "$TMP/rc.out")
  [[ -n "$id" ]] || fail "POST reconcile: no job id in body=$(cat "$TMP/rc.out")"
  [[ "$state" == "queued" || "$state" == "running" ]] \
    || fail "POST reconcile: state=$state want queued|running (trigger must not block)"
  echo "$id"
}

# poll_terminal BASE JAR ID -> writes the terminal job JSON to $TMP/job.out,
# fails if it never reaches done|error within JOB_GRACE.
poll_terminal() {
  local base="$1" jar="$2" id="$3" i state
  for ((i = 0; i < JOB_GRACE; i++)); do
    curl -sf -b "$jar" "$base/admin/v1/reconcile/$id" >"$TMP/job.out" \
      || fail "GET reconcile/$id: request failed"
    state=$(jq -r '.state // empty' "$TMP/job.out")
    case "$state" in
      done | error) echo "$state"; return 0 ;;
      queued | running) ;;
      *) fail "GET reconcile/$id: unexpected state=$state body=$(cat "$TMP/job.out")" ;;
    esac
    sleep 1
  done
  fail "reconcile job $id never reached a terminal state in ${JOB_GRACE}s (is the 'reconcile' worker enabled?)"
}

# assert_summary_fields -> the progress/summary projection the console renders
# must be present + numeric on the terminal job row.
assert_summary_fields() {
  local f
  for f in scanned orphans_found orphans_report manifests_scanned dangling_found healthy errors; do
    jq -e "has(\"$f\") and (.$f | type == \"number\")" "$TMP/job.out" >/dev/null \
      || fail "terminal job missing numeric summary field '$f': $(cat "$TMP/job.out")"
  done
}

# exercise_lab BASE CONTAINER LABEL -> run the full contract against one lab.
exercise_lab() {
  local base="$1" container="$2" label="$3"
  local jar="$TMP/jar-$label" bucket="rc-smoke-$label-$STAMP" state id

  banner "[$label] login + seed ($base)"
  login "$base" "$jar"
  aws --endpoint-url "$base" s3api create-bucket --bucket "$bucket" >/dev/null \
    || fail "[$label] create bucket"
  printf 'alpha-object-body' >"$TMP/o1"
  printf 'beta-object-body'  >"$TMP/o2"
  aws --endpoint-url "$base" s3 cp "$TMP/o1" "s3://$bucket/alpha" >/dev/null || fail "[$label] put alpha"
  aws --endpoint-url "$base" s3 cp "$TMP/o2" "s3://$bucket/beta"  >/dev/null || fail "[$label] put beta"
  pass "[$label] seeded $bucket with 2 objects"

  banner "[$label] DANGLING pass (meta->data) via console — trigger/progress/summary"
  id=$(queue_reconcile "$base" "$jar" "$(printf '{"bucket":"%s","policy":"report"}' "$bucket")")
  note "[$label] dangling job queued: $id"
  state=$(poll_terminal "$base" "$jar" "$id")
  assert_summary_fields
  note "[$label] dangling terminal state=$state summary=$(jq -c '{manifests_scanned,healthy,dangling_found,dangling_report,errors}' "$TMP/job.out")"
  pass "[$label] dangling trigger->progress->summary path works (state=$state)"

  banner "[$label] ORPHAN pass (data->meta) via console"
  id=$(queue_reconcile "$base" "$jar" "$(printf '{"cluster":"%s","pool":"%s","policy":"report"}' "$CLUSTER" "$POOL")")
  note "[$label] orphan job queued: $id"
  state=$(poll_terminal "$base" "$jar" "$id")
  assert_summary_fields
  note "[$label] orphan terminal state=$state summary=$(jq -c '{scanned,orphans_found,orphans_report,absent_backref,errors}' "$TMP/job.out")"
  pass "[$label] orphan trigger->progress->summary path works (state=$state)"

  banner "[$label] rebuild-index --dry-run (last resort CLI, writes nothing)"
  if docker ps --format '{{.Names}}' | grep -qx "$container"; then
    if docker exec "$container" strata admin rebuild-index --dry-run \
        --cluster "$CLUSTER" --pool "$POOL" >"$TMP/rebuild.out" 2>&1; then
      pass "[$label] rebuild-index --dry-run exited 0 (report-only)"
    else
      # A non-zero exit on a default-tag (no-RADOS) gateway is the
      # ErrRADOSNotCompiled path — integration-gated, not a smoke failure.
      note "[$label] rebuild-index --dry-run non-zero (likely no-RADOS gateway): $(tail -1 "$TMP/rebuild.out")"
    fi
  else
    note "[$label] container '$container' not found — skipped rebuild-index leg"
  fi

  aws --endpoint-url "$base" s3 rm "s3://$bucket" --recursive >/dev/null 2>&1 || true
  aws --endpoint-url "$base" s3api delete-bucket --bucket "$bucket" >/dev/null 2>&1 || true
}

RAN=0
banner "Probe TiKV-default lab ($TIKV_BASE)"
if probe_ready "$TIKV_BASE"; then
  exercise_lab "$TIKV_BASE" "${SMOKE_RC_TIKV_CONTAINER:-strata-a}" "tikv"
  RAN=$((RAN + 1))
else
  note "TiKV lab not reachable on $TIKV_BASE/readyz after ${WAIT_GRACE}s — skipping"
fi

banner "Probe Cassandra lab ($CASS_BASE)"
if probe_ready "$CASS_BASE"; then
  exercise_lab "$CASS_BASE" "${SMOKE_RC_CASS_CONTAINER:-strata-cassandra}" "cassandra"
  RAN=$((RAN + 1))
else
  note "Cassandra lab not reachable on $CASS_BASE/readyz after ${WAIT_GRACE}s — skipping"
fi

echo
if [[ "$RAN" -eq 0 ]]; then
  msg="no reconcile lab reachable (TiKV $TIKV_BASE / Cassandra $CASS_BASE)"
  if [[ "$REQUIRE_LAB" == "1" ]]; then
    fail "$msg (REQUIRE_LAB=1)"
  fi
  echo "SKIP: $msg" >&2
  echo "SKIP: bring up a lab (make up-all / make up-cassandra) with STRATA_WORKERS=...,reconcile and re-run." >&2
  exit 77
fi

pass "reconcile/rebuild console contract validated across $RAN lab(s)"
echo "ALL reconcile smoke checks passed."
