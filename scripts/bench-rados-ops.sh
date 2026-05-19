#!/usr/bin/env bash
# RADOS ReadOp / WriteOp batching bench (US-003 of ralph/storage-correctness).
#
# Drives ~10k PUT + ~10k GET against a single-cluster lab with the batched
# helpers (internal/data/rados/ops.go) flipped between off (per-op default,
# STRATA_RADOS_BATCH_OPS=false) and on (=true). Reads p50/p95/p99 from
# Prometheus via histogram_quantile() against
# strata_rados_op_duration_seconds_bucket and prints both passes' numbers
# plus a verdict — SHIP_BATCHED if p99 PUT batched ≤90% of baseline (≥10%
# improvement, matches the US-003 ship gate), HOLD_DEFAULT otherwise.
#
# The script does NOT bring up the lab. Operator stands the stack up with:
#
#   make up-all && make wait-tikv && make wait-ceph && make wait-strata-lab
#
# Replicas are restarted between passes via BENCH_RESTART_HOOK (default:
# docker compose force-recreate of strata-a/b with the env injected).
# Override if your lab uses a different restart recipe.
#
# Pre-reqs on the host: docker, curl, jq, aws (>=2). Lab gateway reachable
# on $BASE (default http://127.0.0.1:9999 — nginx LB port for the
# bare-default lab). STRATA_STATIC_CREDENTIALS exported with the same
# value the gateway booted with. Prometheus reachable on $PROM (default
# http://127.0.0.1:9090) — see deploy/docker/docker-compose.yml.
#
# Skip behaviour: when lab is not reachable after WAIT_GRACE seconds the
# script EXITs 77. Set REQUIRE_LAB=1 to convert the skip into a hard fail.

set -euo pipefail

BASE="${BASE:-http://127.0.0.1:9999}"
PROM="${PROM:-http://127.0.0.1:9090}"
WAIT_GRACE="${WAIT_GRACE:-30}"
REQUIRE_LAB="${REQUIRE_LAB:-0}"

BENCH_OBJECT_COUNT="${BENCH_OBJECT_COUNT:-10000}"
BENCH_OBJECT_SIZE_KB="${BENCH_OBJECT_SIZE_KB:-32}"
BENCH_CONCURRENCY="${BENCH_CONCURRENCY:-16}"
BENCH_RESULTS_FILE="${BENCH_RESULTS_FILE:-bench-rados-ops-results.jsonl}"
BENCH_SHIP_GATE_PCT="${BENCH_SHIP_GATE_PCT:-90}"

COMPOSE_FILE="${COMPOSE_FILE:-deploy/docker/docker-compose.yml}"
COMPOSE_CMD="${COMPOSE_CMD:-docker compose -f $COMPOSE_FILE}"
RESTART_CONTAINERS="${RESTART_CONTAINERS:-strata-a strata-b}"

BENCH_RESTART_HOOK="${BENCH_RESTART_HOOK:-STRATA_RADOS_BATCH_OPS=\$BATCH $COMPOSE_CMD up -d --force-recreate $RESTART_CONTAINERS}"

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
RUN_TAG="$(date +%s)"
BUCKET="bench-rados-ops-$RUN_TAG"

fail() { echo "FAIL: $*" >&2; exit 1; }
pass() { echo "PASS: $*"; }
note() { echo "INFO: $*"; }

aws_creds() {
  export AWS_ACCESS_KEY_ID="$AK"
  export AWS_SECRET_ACCESS_KEY="$SK"
  export AWS_DEFAULT_REGION="${AWS_DEFAULT_REGION:-us-east-1}"
  export AWS_EC2_METADATA_DISABLED=true
}

cleanup() {
  aws --endpoint-url "$BASE" s3 rm "s3://$BUCKET" --recursive >/dev/null 2>&1 || true
  aws --endpoint-url "$BASE" s3api delete-bucket --bucket "$BUCKET" >/dev/null 2>&1 || true
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
  echo "SKIP: bring up lab via 'make up-all' (see header) and re-run." >&2
  exit 77
fi

restart_with_batch() {
  local BATCH="$1"
  export BATCH
  note "restarting replicas with STRATA_RADOS_BATCH_OPS=$BATCH"
  if ! eval "$BENCH_RESTART_HOOK"; then
    fail "restart hook failed (cmd: $BENCH_RESTART_HOOK)"
  fi
  unset BATCH
  local i=0
  while (( i < 120 )); do
    if [[ "$(curl -fsS -o /dev/null -w '%{http_code}' "$BASE/readyz")" == "200" ]]; then
      note "lab back online"
      return 0
    fi
    sleep 2
    i=$((i+2))
  done
  fail "lab did not come back ready within 240s after restart"
}

seed_bucket() {
  aws --endpoint-url "$BASE" s3api create-bucket --bucket "$BUCKET" >/dev/null 2>&1 || true
}

run_put_workload() {
  local count="$1" sizekb="$2" concurrency="$3"
  local payload="$TMP/payload.bin"
  dd if=/dev/urandom of="$payload" bs=1024 count="$sizekb" 2>/dev/null
  note "PUT workload: $count objects × ${sizekb} KiB, concurrency=$concurrency"
  local start_s
  start_s=$(date +%s)
  seq 0 $((count-1)) | xargs -P "$concurrency" -I{} \
    aws --endpoint-url "$BASE" s3 cp "$payload" "s3://$BUCKET/put-{}.bin" >/dev/null 2>&1
  local elapsed=$(( $(date +%s) - start_s ))
  note "PUT wall-clock: ${elapsed}s"
  echo "$elapsed"
}

run_get_workload() {
  local count="$1" concurrency="$2"
  note "GET workload: $count objects, concurrency=$concurrency"
  local start_s
  start_s=$(date +%s)
  seq 0 $((count-1)) | xargs -P "$concurrency" -I{} \
    aws --endpoint-url "$BASE" s3 cp "s3://$BUCKET/put-{}.bin" "$TMP/get-{}.bin" >/dev/null 2>&1
  local elapsed=$(( $(date +%s) - start_s ))
  note "GET wall-clock: ${elapsed}s"
  echo "$elapsed"
}

prom_query() {
  local q="$1"
  curl -fsS --get --data-urlencode "query=$q" "$PROM/api/v1/query" \
    | jq -r '.data.result[0].value[1] // "NaN"'
}

quantile() {
  local quant="$1" op="$2"
  prom_query "histogram_quantile($quant, sum by (le) (rate(strata_rados_op_duration_seconds_bucket{op=\"$op\"}[5m])))"
}

run_pass() {
  local batch="$1"
  echo
  echo "== Pass batch=$batch"
  restart_with_batch "$batch"
  seed_bucket
  local put_s get_s
  put_s=$(run_put_workload "$BENCH_OBJECT_COUNT" "$BENCH_OBJECT_SIZE_KB" "$BENCH_CONCURRENCY")
  # Let Prom scrape the histogram before reading quantiles.
  sleep 30
  local put_p50 put_p95 put_p99
  put_p50=$(quantile 0.5  put)
  put_p95=$(quantile 0.95 put)
  put_p99=$(quantile 0.99 put)
  get_s=$(run_get_workload "$BENCH_OBJECT_COUNT" "$BENCH_CONCURRENCY")
  sleep 30
  local get_p50 get_p95 get_p99
  get_p50=$(quantile 0.5  get)
  get_p95=$(quantile 0.95 get)
  get_p99=$(quantile 0.99 get)
  # Clear the bucket between passes so the next seed starts from 0 objects.
  aws --endpoint-url "$BASE" s3 rm "s3://$BUCKET" --recursive >/dev/null 2>&1 || true
  printf '{"batch":%s,"put_s":%d,"get_s":%d,"put_p50":%s,"put_p95":%s,"put_p99":%s,"get_p50":%s,"get_p95":%s,"get_p99":%s}\n' \
    "$batch" "$put_s" "$get_s" \
    "$put_p50" "$put_p95" "$put_p99" \
    "$get_p50" "$get_p95" "$get_p99" \
    | tee -a "$BENCH_RESULTS_FILE"
}

aws_creds

rm -f "$BENCH_RESULTS_FILE"
note "results JSONL: $BENCH_RESULTS_FILE"

run_pass false
run_pass true

# Ship gate: batched p99 PUT ≤ BENCH_SHIP_GATE_PCT% of baseline ⇒ SHIP_BATCHED.
BASELINE_P99=$(jq -r 'select(.batch==false) | .put_p99' < "$BENCH_RESULTS_FILE" | tail -n 1)
BATCHED_P99=$(jq  -r 'select(.batch==true)  | .put_p99' < "$BENCH_RESULTS_FILE" | tail -n 1)

echo
echo "== Summary"
printf "  baseline PUT p99 = %s\n" "$BASELINE_P99"
printf "  batched  PUT p99 = %s\n" "$BATCHED_P99"

if [[ "$BASELINE_P99" == "NaN" || "$BATCHED_P99" == "NaN" ]]; then
  echo "VERDICT: NO_METRICS (Prom did not return p99 — confirm rados pool is being hit)"
  exit 0
fi

RATIO_PCT=$(awk -v a="$BATCHED_P99" -v b="$BASELINE_P99" 'BEGIN { if (b == 0) print 100; else printf "%d\n", (a / b) * 100 }')
printf "  ratio            = %s %% (ship gate ≤%s %%)\n" "$RATIO_PCT" "$BENCH_SHIP_GATE_PCT"

if (( RATIO_PCT <= BENCH_SHIP_GATE_PCT )); then
  echo "VERDICT: SHIP_BATCHED (batching reduces p99 PUT by ≥$((100-BENCH_SHIP_GATE_PCT))%)"
else
  echo "VERDICT: HOLD_DEFAULT (batching gain below ship threshold; keeping STRATA_RADOS_BATCH_OPS=false default — see docs/site/content/architecture/benchmarks/rados-ops.md)"
fi
