#!/usr/bin/env bash
# TiKV-default 2-replica lab walkthrough smoke (US-005 of
# ralph/tikv-default-lab). Closes ROADMAP P3 "TiKV-default 2-replica lab"
# by driving the four operator scenarios end-to-end against a running
# compose stack.
#
# Scenarios:
#
#   A. Bare bring-up — TiKV default:
#      - Assume `docker compose up -d` already ran; probe :9999/readyz.
#      - Assert services running: pd, tikv, ceph, ceph-b, strata-a,
#        strata-b, strata-lb-nginx, prometheus, grafana.
#      - Assert retired containers do NOT exist: strata-tikv-a / b / c,
#        strata-cass-a / b / c, strata-lb-nginx-cass.
#      - 20-request authenticated burst through :9999; assert each
#        replica got >= 5 hits via `docker logs strata-{a,b} --since`.
#      - PUT 10 objects, GET them all (round-trip).
#      - Drain cephb evacuate; assert deregister_ready=true within
#        SMOKE_TDL_DRAIN_TIMEOUT_S (default 300).
#      - Best-effort undrain on exit so the lab stays usable.
#
#   B. Cassandra profile side-by-side:
#      - Probe :9998/readyz. If unreachable, SKIP with operator note
#        (start via `make up-cassandra && make wait-cassandra`).
#      - Assert ADDITIONAL services: cassandra, strata-cassandra.
#      - PUT 10 objects via :9998 and :9999 each (distinct bucket names).
#      - Assert independent metadata: TiKV bucket NOT listed on
#        Cassandra LB, Cassandra bucket NOT listed on TiKV LB.
#
#   C. Env-override single-cluster on TiKV default:
#      - Opt-in via SMOKE_TDL_SCENARIO_C=1; otherwise SKIP with operator
#        recipe (bring up strata-a/b/lb with STRATA_RADOS_CLUSTERS=default:...).
#      - When opt-in: PUT 1 object via :9999, GET round-trip green; check
#        strata-a logs lack `cluster=cephb` boot lines.
#
#   D. Residue grep gate:
#      - From the repo root, grep for retired profile / service names
#        outside the documented exception set (archive, public, resources,
#        progress.txt / prd.json, the smoke script itself, ROADMAP).
#      - Zero matches required.
#
# Skip behaviour: exit 77 when :9999/readyz unreachable; REQUIRE_LAB=1
# turns the skip into a hard fail.

set -euo pipefail

BASE="${BASE:-http://127.0.0.1:9999}"
CASS_BASE="${CASS_BASE:-http://127.0.0.1:9998}"
WAIT_GRACE="${WAIT_GRACE:-15}"
REQUIRE_LAB="${REQUIRE_LAB:-0}"
DRAIN_TIMEOUT_S="${SMOKE_TDL_DRAIN_TIMEOUT_S:-300}"
SCENARIO_C="${SMOKE_TDL_SCENARIO_C:-0}"
CLUSTER_DRAIN="${SMOKE_TDL_CLUSTER:-cephb}"
STAMP="$(date +%s)"

# Bare-default services (no profile flag) per US-001 compose config.
EXPECTED_BARE_SERVICES=(pd tikv ceph ceph-b strata-a strata-b strata-lb-nginx prometheus grafana)
# Containers under --profile cassandra.
EXPECTED_CASS_SERVICES=(cassandra strata-cassandra)
# Container names that MUST NOT exist (retired by this cycle).
RETIRED_CONTAINERS=(strata-tikv-a strata-tikv-b strata-tikv-c strata-cass-a strata-cass-b strata-cass-c strata-lb-nginx-cass)

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
JAR_CASS="$TMP/cookies-cass"
CLEANUP_BUCKETS=()
CLEANUP_BUCKETS_CASS=()
UNDRAIN_ON_EXIT=0

fail() { echo "FAIL: $*" >&2; exit 1; }
pass() { echo "PASS: $*"; }
note() { echo "INFO: $*"; }
banner() { echo; echo "==> $*"; }
skipped() { echo "SKIP: $*"; }

cleanup() {
  if (( UNDRAIN_ON_EXIT == 1 )); then
    curl -sf -b "$JAR" -X POST "$BASE/admin/v1/clusters/$CLUSTER_DRAIN/undrain" >/dev/null 2>&1 || true
  fi
  for b in "${CLEANUP_BUCKETS[@]}"; do
    AWS_ACCESS_KEY_ID="$AK" AWS_SECRET_ACCESS_KEY="$SK" \
      aws --endpoint-url "$BASE" s3 rm "s3://$b" --recursive >/dev/null 2>&1 || true
    AWS_ACCESS_KEY_ID="$AK" AWS_SECRET_ACCESS_KEY="$SK" \
      aws --endpoint-url "$BASE" s3api delete-bucket --bucket "$b" >/dev/null 2>&1 || true
  done
  for b in "${CLEANUP_BUCKETS_CASS[@]}"; do
    AWS_ACCESS_KEY_ID="$AK" AWS_SECRET_ACCESS_KEY="$SK" \
      aws --endpoint-url "$CASS_BASE" s3 rm "s3://$b" --recursive >/dev/null 2>&1 || true
    AWS_ACCESS_KEY_ID="$AK" AWS_SECRET_ACCESS_KEY="$SK" \
      aws --endpoint-url "$CASS_BASE" s3api delete-bucket --bucket "$b" >/dev/null 2>&1 || true
  done
  rm -rf "$TMP"
}
trap cleanup EXIT

probe_ready() {
  local base="$1" grace="${2:-$WAIT_GRACE}"
  local i=0
  while (( i < grace )); do
    if [[ "$(curl -fsS -o /dev/null -w '%{http_code}' "$base/readyz")" == "200" ]]; then
      return 0
    fi
    sleep 1
    i=$((i+1))
  done
  return 1
}

if ! probe_ready "$BASE"; then
  msg="bare-default lab not reachable on $BASE/readyz after ${WAIT_GRACE}s"
  if [[ "$REQUIRE_LAB" == "1" ]]; then
    fail "$msg (REQUIRE_LAB=1)"
  fi
  echo "SKIP: $msg" >&2
  echo "SKIP: bring up via 'make up && make wait-strata-lab' then re-run." >&2
  exit 77
fi

login() {
  local base="$1" jar="$2"
  local body code
  body=$(printf '{"access_key":"%s","secret_key":"%s"}' "$AK" "$SK")
  code=$(curl -sS -o "$TMP/login.out" -w '%{http_code}' \
    -c "$jar" -H 'Content-Type: application/json' \
    -X POST -d "$body" "$base/admin/v1/auth/login")
  [[ "$code" == "200" ]] || fail "login $base: HTTP $code body=$(cat "$TMP/login.out")"
}

aws_creds() {
  export AWS_ACCESS_KEY_ID="$AK"
  export AWS_SECRET_ACCESS_KEY="$SK"
  export AWS_DEFAULT_REGION="${AWS_DEFAULT_REGION:-us-east-1}"
  export AWS_EC2_METADATA_DISABLED=true
}

aws_creds
login "$BASE" "$JAR"

PAYLOAD="$TMP/payload.bin"
dd if=/dev/urandom of="$PAYLOAD" bs=1024 count=1 2>/dev/null
PAYLOAD_MD5="$(md5sum "$PAYLOAD" 2>/dev/null | awk '{print $1}' || md5 -q "$PAYLOAD")"

# ===================================================================
# Scenario A — bare bring-up (TiKV default)
# ===================================================================
banner "Scenario A: bare bring-up — TiKV default"

note "Assert bare-default service set running"
RUNNING="$(docker ps --format '{{.Names}}')"
for svc in "${EXPECTED_BARE_SERVICES[@]}"; do
  # service names map to container names; ceph-b → strata-cephb.
  case "$svc" in
    pd) cn="strata-pd" ;;
    tikv) cn="strata-tikv-storage" ;;
    ceph) cn="strata-ceph" ;;
    ceph-b) cn="strata-cephb" ;;
    strata-a) cn="strata-a" ;;
    strata-b) cn="strata-b" ;;
    strata-lb-nginx) cn="strata-lb-nginx" ;;
    prometheus) cn="strata-prometheus" ;;
    grafana) cn="strata-grafana" ;;
    *) cn="$svc" ;;
  esac
  echo "$RUNNING" | grep -qx "$cn" \
    || fail "A:expected container '$cn' (service '$svc') not running"
done
pass "A:bare-default ${#EXPECTED_BARE_SERVICES[@]} services running"

note "Assert retired containers absent"
for cn in "${RETIRED_CONTAINERS[@]}"; do
  if echo "$RUNNING" | grep -qx "$cn"; then
    fail "A:retired container '$cn' is running — cycle rename incomplete"
  fi
done
pass "A:retired containers absent (${#RETIRED_CONTAINERS[@]} checked)"

note "20-request authenticated burst through LB :9999"
# Timestamp captured before the burst so `docker logs --since` covers it.
BURST_START="$(date +%s)"
for i in $(seq 1 20); do
  code=$(curl -sS -o /dev/null -w '%{http_code}' -b "$JAR" "$BASE/admin/v1/clusters")
  [[ "$code" == "200" ]] || fail "A:burst req $i expected 200 got $code"
done
sleep 2  # give logs time to flush
NOW="$(date +%s)"
SINCE="$(( NOW - BURST_START + 5 ))s"
A_LOGS="$(docker logs strata-a --since "$SINCE" 2>&1 | grep -c '/admin/v1/clusters' || true)"
B_LOGS="$(docker logs strata-b --since "$SINCE" 2>&1 | grep -c '/admin/v1/clusters' || true)"
note "A:burst log hits — strata-a=$A_LOGS strata-b=$B_LOGS"
(( A_LOGS >= 5 )) || fail "A:strata-a saw $A_LOGS GETs in burst (expected >= 5 — LB round-robin broken?)"
(( B_LOGS >= 5 )) || fail "A:strata-b saw $B_LOGS GETs in burst (expected >= 5 — LB round-robin broken?)"
pass "A:LB :9999 round-robins across strata-a ($A_LOGS hits) + strata-b ($B_LOGS hits)"

note "PUT 10 objects + GET round-trip via :9999"
A_BUCKET="tdl-a-$STAMP"
aws --endpoint-url "$BASE" s3api create-bucket --bucket "$A_BUCKET" >/dev/null \
  || fail "A:create-bucket $A_BUCKET failed"
CLEANUP_BUCKETS+=("$A_BUCKET")
for i in $(seq 1 10); do
  aws --endpoint-url "$BASE" s3 cp "$PAYLOAD" "s3://$A_BUCKET/obj-$i.bin" >/dev/null \
    || fail "A:PUT obj-$i.bin failed"
done
for i in $(seq 1 10); do
  aws --endpoint-url "$BASE" s3 cp "s3://$A_BUCKET/obj-$i.bin" "$TMP/got-$i.bin" >/dev/null \
    || fail "A:GET obj-$i.bin failed"
  GOT_MD5="$(md5sum "$TMP/got-$i.bin" 2>/dev/null | awk '{print $1}' || md5 -q "$TMP/got-$i.bin")"
  [[ "$GOT_MD5" == "$PAYLOAD_MD5" ]] \
    || fail "A:GET obj-$i.bin md5 mismatch got=$GOT_MD5 want=$PAYLOAD_MD5"
done
pass "A:PUT/GET round-trip green (10 objects)"

note "Drain $CLUSTER_DRAIN evacuate"
DRAIN_BODY='{"mode":"evacuate"}'
DRAIN_CODE=$(curl -sS -o "$TMP/drain.out" -w '%{http_code}' \
  -b "$JAR" -H 'Content-Type: application/json' \
  -X POST -d "$DRAIN_BODY" "$BASE/admin/v1/clusters/$CLUSTER_DRAIN/drain")
[[ "$DRAIN_CODE" == "200" || "$DRAIN_CODE" == "204" ]] \
  || fail "A:drain expected 200/204 got $DRAIN_CODE body=$(cat "$TMP/drain.out")"
UNDRAIN_ON_EXIT=1
pass "A:drain $CLUSTER_DRAIN evacuate accepted"

note "Wait deregister_ready=true (timeout ${DRAIN_TIMEOUT_S}s)"
deadline=$(( $(date +%s) + DRAIN_TIMEOUT_S ))
while (( $(date +%s) < deadline )); do
  ready=$(curl -sf -b "$JAR" "$BASE/admin/v1/clusters/$CLUSTER_DRAIN/drain-progress" \
    | jq -r '.deregister_ready // false')
  if [[ "$ready" == "true" ]]; then
    pass "A:drain converged (deregister_ready=true)"
    break
  fi
  sleep 5
done
if [[ "$ready" != "true" ]]; then
  fail "A:deregister_ready stayed false after ${DRAIN_TIMEOUT_S}s"
fi

# Eager undrain so subsequent scenarios + future smoke runs work.
curl -sf -b "$JAR" -X POST "$BASE/admin/v1/clusters/$CLUSTER_DRAIN/undrain" >/dev/null 2>&1 || true
UNDRAIN_ON_EXIT=0
note "A:undrain issued — cluster $CLUSTER_DRAIN restored to live"

# ===================================================================
# Scenario B — Cassandra profile side-by-side
# ===================================================================
banner "Scenario B: Cassandra profile side-by-side"

if ! probe_ready "$CASS_BASE" 5; then
  skipped "B:Cassandra strata not reachable on $CASS_BASE/readyz — bring up via 'make up-cassandra && make wait-cassandra' to exercise this scenario"
else
  note "Probe Cassandra-profile services running"
  for svc in "${EXPECTED_CASS_SERVICES[@]}"; do
    case "$svc" in
      cassandra) cn="strata-cassandra-db" ;;
      strata-cassandra) cn="strata-cassandra" ;;
      *) cn="$svc" ;;
    esac
    echo "$RUNNING" | grep -qx "$cn" \
      || fail "B:expected container '$cn' (service '$svc') not running"
  done
  pass "B:cassandra + strata-cassandra running"

  login "$CASS_BASE" "$JAR_CASS"

  # Distinct bucket names so we can verify metadata isolation:
  # the TiKV bucket must NOT appear on the Cassandra LB and vice versa.
  B_TIKV="tdl-b-tikv-$STAMP"
  B_CASS="tdl-b-cass-$STAMP"
  aws --endpoint-url "$BASE" s3api create-bucket --bucket "$B_TIKV" >/dev/null \
    || fail "B:create $B_TIKV on TiKV failed"
  CLEANUP_BUCKETS+=("$B_TIKV")
  aws --endpoint-url "$CASS_BASE" s3api create-bucket --bucket "$B_CASS" >/dev/null \
    || fail "B:create $B_CASS on Cassandra failed"
  CLEANUP_BUCKETS_CASS+=("$B_CASS")

  for i in $(seq 1 10); do
    aws --endpoint-url "$BASE" s3 cp "$PAYLOAD" "s3://$B_TIKV/k-$i.bin" >/dev/null \
      || fail "B:PUT TiKV k-$i.bin failed"
    aws --endpoint-url "$CASS_BASE" s3 cp "$PAYLOAD" "s3://$B_CASS/k-$i.bin" >/dev/null \
      || fail "B:PUT Cassandra k-$i.bin failed"
  done
  pass "B:10 objects PUT on each backend"

  TIKV_LIST="$(aws --endpoint-url "$BASE" s3api list-buckets | jq -r '.Buckets[].Name')"
  CASS_LIST="$(aws --endpoint-url "$CASS_BASE" s3api list-buckets | jq -r '.Buckets[].Name')"
  echo "$TIKV_LIST" | grep -qx "$B_TIKV" \
    || fail "B:TiKV ListBuckets missing $B_TIKV"
  echo "$CASS_LIST" | grep -qx "$B_CASS" \
    || fail "B:Cassandra ListBuckets missing $B_CASS"
  if echo "$TIKV_LIST" | grep -qx "$B_CASS"; then
    fail "B:Cassandra bucket $B_CASS leaked into TiKV ListBuckets — metadata not isolated"
  fi
  if echo "$CASS_LIST" | grep -qx "$B_TIKV"; then
    fail "B:TiKV bucket $B_TIKV leaked into Cassandra ListBuckets — metadata not isolated"
  fi
  pass "B:metadata isolated — TiKV + Cassandra each see only their own bucket"
fi

# ===================================================================
# Scenario C — env-override single-cluster on TiKV default
# ===================================================================
banner "Scenario C: env-override single-cluster on TiKV default"

if [[ "$SCENARIO_C" != "1" ]]; then
  skipped "C:SMOKE_TDL_SCENARIO_C != 1 — opt-in via 'STRATA_RADOS_CLUSTERS=default:/etc/ceph-a/ceph.conf:/etc/ceph-a/ceph.client.admin.keyring docker compose up -d --force-recreate strata-a strata-b strata-lb-nginx' + 'SMOKE_TDL_SCENARIO_C=1 make smoke-tikv-default-lab'"
else
  note "C:opt-in PUT/GET round-trip against single-cluster lab"
  C_BUCKET="tdl-c-$STAMP"
  aws --endpoint-url "$BASE" s3api create-bucket --bucket "$C_BUCKET" >/dev/null \
    || fail "C:create-bucket $C_BUCKET failed"
  CLEANUP_BUCKETS+=("$C_BUCKET")
  aws --endpoint-url "$BASE" s3 cp "$PAYLOAD" "s3://$C_BUCKET/c.bin" >/dev/null \
    || fail "C:PUT c.bin failed"
  aws --endpoint-url "$BASE" s3 cp "s3://$C_BUCKET/c.bin" "$TMP/got-c.bin" >/dev/null \
    || fail "C:GET c.bin failed"
  GOT_MD5="$(md5sum "$TMP/got-c.bin" 2>/dev/null | awk '{print $1}' || md5 -q "$TMP/got-c.bin")"
  [[ "$GOT_MD5" == "$PAYLOAD_MD5" ]] \
    || fail "C:GET c.bin md5 mismatch got=$GOT_MD5 want=$PAYLOAD_MD5"
  pass "C:PUT/GET round-trip green on single-cluster lab"

  # Check strata-a logs lack a cephb boot-time connection line.
  # When STRATA_RADOS_CLUSTERS only carries `default:...`, the cephb
  # cluster id never appears in the boot log.
  if docker logs strata-a 2>&1 | grep -E 'cluster[ =]"?cephb"?' >/dev/null; then
    fail "C:strata-a logs reference 'cephb' — env-override did NOT collapse to single-cluster"
  fi
  pass "C:strata-a logs show only single-cluster boot"
fi

# ===================================================================
# Scenario D — residue grep gate
# ===================================================================
banner "Scenario D: residue grep gate"

REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null || true)"
if [[ -z "$REPO_ROOT" ]]; then
  skipped "D:not inside a git repo — grep gate requires a repo root"
else
  cd "$REPO_ROOT"
  PATTERN='strata-tikv-a|strata-tikv-b|strata-tikv-c|strata-cass-|strata-lb-nginx-cass|--profile lab-tikv|--profile lab-cassandra-3|--profile tikv'
  # Exception set (per US-003):
  #   scripts/ralph/archive/**, docs/site/public/**, docs/site/resources/**,
  #   scripts/ralph/progress.txt, scripts/ralph/prd.json,
  #   the smoke script itself, ROADMAP.md (close-flip narratives), and
  #   docs/site/content/architecture/migrations/** (migration notes are
  #   narrative documentation that name the retired shapes by design).
  HITS="$(git grep -nE "$PATTERN" -- \
    ':!scripts/ralph/archive' \
    ':!docs/site/public' \
    ':!docs/site/resources' \
    ':!docs/site/content/architecture/migrations' \
    ':!scripts/ralph/progress.txt' \
    ':!scripts/ralph/prd.json' \
    ':!scripts/smoke-tikv-default-lab.sh' \
    ':!ROADMAP.md' \
    || true)"
  if [[ -n "$HITS" ]]; then
    echo "$HITS" >&2
    fail "D:residue grep returned non-empty — retired profile / service names still present"
  fi
  pass "D:zero residue matches outside documented exception set"
fi

echo
echo "== tikv-default-lab smoke OK (Scenarios A + B + C + D green or skipped per opt-in)"
