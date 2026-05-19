#!/usr/bin/env bash
# OTel ring-buffer bytes-budget bench (US-005 of ralph/storage-correctness).
#
# Starts `make run-memory` with STRATA_OTEL_RINGBUF=on and varying
# STRATA_OTEL_RINGBUF_BYTES ∈ {4, 8, 16, 32} MiB. For each budget,
# drives 60 s of burst HTTP traffic at concurrency 100 via `hey` against
# the in-memory backend (so the trace producer is the cheapest possible
# path — every request emits a root span + a few children). Reads
# Prometheus counters/gauges scraped from the strata replica's /metrics
# endpoint directly (no separate Prometheus needed) and computes:
#
#   (a) eviction rate    — `strata_otel_ringbuf_evicted_total` delta / 60 s
#   (b) retention horizon — `strata_otel_ringbuf_oldest_age_seconds` gauge
#                          (LRU-back retention age)
#   (c) memory ceiling    — `process_resident_memory_bytes` peak gauge
#
# Prints one JSONL row per pass + a final verdict line:
#
#   SHIP_16MIB    — bumping default to 16 MiB raises retained-trace-age
#                   by ≥ 30 % vs 4 MiB without ≥ 2× memory hit. Flip the
#                   `ringbuf.DefaultBytesBudget` in
#                   internal/otel/ringbuf/ringbuf.go to 16 << 20.
#   HOLD_DEFAULT  — gate not crossed; keep 4 MiB + surface the env knob
#                   more prominently in docs/site/content/best-practices/
#                   monitoring.md.
#
# The script does NOT manage compose. Operator runs the binary in-process
# via `make run-memory` (or a custom recipe via $BENCH_RUN_HOOK).
#
# Pre-reqs on the host:
#   - `make build` ran (script invokes ./bin/strata directly via the hook)
#   - `hey` HTTP load generator (https://github.com/rakyll/hey)
#   - `curl`, `jq`, `awk`, `lsof`
#
# Skip behaviour: when the gateway is not reachable after WAIT_GRACE
# seconds the script EXITs 77. Set REQUIRE_LAB=1 to convert into hard
# fail.

set -euo pipefail

BASE="${BASE:-http://127.0.0.1:9999}"
WAIT_GRACE="${WAIT_GRACE:-30}"
REQUIRE_LAB="${REQUIRE_LAB:-0}"

BENCH_DURATION="${BENCH_DURATION:-60s}"
BENCH_CONCURRENCY="${BENCH_CONCURRENCY:-100}"
BENCH_RESULTS_FILE="${BENCH_RESULTS_FILE:-bench-otel-ringbuf-results.jsonl}"
BENCH_BYTES_MIB="${BENCH_BYTES_MIB:-4 8 16 32}"

# Ship gate: 16 MiB retention age ≥ 130 % of 4 MiB, AND 16 MiB rss ≤ 200 %
# of 4 MiB rss. Below either threshold → HOLD_DEFAULT.
BENCH_SHIP_RETENTION_PCT="${BENCH_SHIP_RETENTION_PCT:-130}"
BENCH_SHIP_RSS_PCT="${BENCH_SHIP_RSS_PCT:-200}"

# Defaults to `make run-memory`. Override if you need a different recipe
# (e.g. cassandra-backed). The hook MUST exit when the env signals SIGTERM;
# `make` proxies signals via go's stdlib.
BENCH_RUN_HOOK="${BENCH_RUN_HOOK:-make run-memory}"

for tool in curl jq awk hey; do
  command -v "$tool" >/dev/null 2>&1 || { echo "FAIL: missing tool: $tool" >&2; exit 2; }
done

TMP="$(mktemp -d)"
fail() { echo "FAIL: $*" >&2; exit 1; }
note() { echo "INFO: $*"; }

cleanup() {
  if [[ -n "${RUN_PID:-}" ]] && kill -0 "$RUN_PID" 2>/dev/null; then
    kill -TERM "$RUN_PID" 2>/dev/null || true
    wait "$RUN_PID" 2>/dev/null || true
  fi
  rm -rf "$TMP"
}
trap cleanup EXIT

probe_ready() {
  local i=0
  while (( i < WAIT_GRACE )); do
    if [[ "$(curl -fsS -o /dev/null -w '%{http_code}' "$BASE/healthz")" == "200" ]]; then
      return 0
    fi
    sleep 1
    i=$((i+1))
  done
  return 1
}

start_strata() {
  local bytes_mib="$1"
  local bytes=$(( bytes_mib * 1024 * 1024 ))
  note "starting strata with STRATA_OTEL_RINGBUF_BYTES=${bytes} (${bytes_mib} MiB)"
  STRATA_OTEL_RINGBUF=on \
  STRATA_OTEL_RINGBUF_BYTES="$bytes" \
  STRATA_LOG_LEVEL=WARN \
    bash -c "$BENCH_RUN_HOOK" >"$TMP/strata.$bytes_mib.log" 2>&1 &
  RUN_PID=$!
  if ! probe_ready; then
    local msg="gateway not reachable on $BASE/healthz after ${WAIT_GRACE}s"
    if [[ "$REQUIRE_LAB" == "1" ]]; then
      fail "$msg (REQUIRE_LAB=1)"
    fi
    echo "SKIP: $msg" >&2
    exit 77
  fi
}

stop_strata() {
  if [[ -n "${RUN_PID:-}" ]] && kill -0 "$RUN_PID" 2>/dev/null; then
    kill -TERM "$RUN_PID" 2>/dev/null || true
    wait "$RUN_PID" 2>/dev/null || true
  fi
  RUN_PID=""
}

metric_value() {
  # Reads one Prom metric value from /metrics. Sums every series so a
  # labelled counter (e.g. WorkerPanicTotal) collapses into a single
  # number — for our use-case, the metrics of interest carry no labels
  # so the sum reduces to the value.
  local name="$1"
  curl -fsS "$BASE/metrics" \
    | awk -v m="$name" '
        $1 == m { v += $2 }
        $1 ~ "^"m"{" { v += $2 }
        END { if (v == "") print 0; else print v }
      '
}

run_pass() {
  local bytes_mib="$1"
  echo
  echo "== Pass bytes_mib=$bytes_mib"
  start_strata "$bytes_mib"

  # Seed a single bucket so the GET hot path actually probes a real key.
  curl -fsS -X PUT "$BASE/bench-ringbuf-bkt" >/dev/null 2>&1 || true

  # Drive 60 s of burst GET traffic — every request emits a root span +
  # auth + meta child spans, so the ring fills quickly.
  local ev_before mem_before
  ev_before=$(metric_value strata_otel_ringbuf_evicted_total)
  mem_before=$(metric_value process_resident_memory_bytes)

  note "hey -z $BENCH_DURATION -c $BENCH_CONCURRENCY $BASE/bench-ringbuf-bkt/probe"
  hey -z "$BENCH_DURATION" -c "$BENCH_CONCURRENCY" -disable-keepalive \
    "$BASE/bench-ringbuf-bkt/probe" >/dev/null 2>&1 || true

  # Settle so the gauge reflects the post-workload state.
  sleep 2

  local ev_after age_after mem_after traces_after
  ev_after=$(metric_value strata_otel_ringbuf_evicted_total)
  age_after=$(metric_value strata_otel_ringbuf_oldest_age_seconds)
  mem_after=$(metric_value process_resident_memory_bytes)
  traces_after=$(metric_value strata_otel_ringbuf_traces)

  local evictions
  evictions=$(awk -v a="$ev_after" -v b="$ev_before" 'BEGIN { printf "%d\n", a - b }')

  printf '{"bytes_mib":%d,"evictions":%d,"oldest_age_seconds":%s,"rss_bytes":%s,"rss_bytes_before":%s,"traces_retained":%s}\n' \
    "$bytes_mib" "$evictions" "$age_after" "$mem_after" "$mem_before" "$traces_after" \
    | tee -a "$BENCH_RESULTS_FILE"

  stop_strata
}

rm -f "$BENCH_RESULTS_FILE"
note "results JSONL: $BENCH_RESULTS_FILE"

for mib in $BENCH_BYTES_MIB; do
  run_pass "$mib"
done

# Ship gate: 16 MiB retention age ≥ BENCH_SHIP_RETENTION_PCT% of 4 MiB
# AND 16 MiB rss ≤ BENCH_SHIP_RSS_PCT% of 4 MiB rss.
BASE_AGE=$(jq -r 'select(.bytes_mib==4) | .oldest_age_seconds' < "$BENCH_RESULTS_FILE" | tail -n 1)
BASE_RSS=$(jq -r 'select(.bytes_mib==4) | .rss_bytes' < "$BENCH_RESULTS_FILE" | tail -n 1)
TGT_AGE=$(jq -r 'select(.bytes_mib==16) | .oldest_age_seconds' < "$BENCH_RESULTS_FILE" | tail -n 1)
TGT_RSS=$(jq -r 'select(.bytes_mib==16) | .rss_bytes' < "$BENCH_RESULTS_FILE" | tail -n 1)

echo
echo "== Summary"
printf "  bytes_mib=4  oldest_age_seconds = %s   rss_bytes = %s\n" "$BASE_AGE" "$BASE_RSS"
printf "  bytes_mib=16 oldest_age_seconds = %s   rss_bytes = %s\n" "$TGT_AGE" "$TGT_RSS"

if [[ -z "$BASE_AGE" || "$BASE_AGE" == "null" || "$TGT_AGE" == "null" || "$BASE_AGE" == "0" ]]; then
  echo "VERDICT: NO_METRICS (gauge zero — confirm the gateway is emitting traces during the load run)"
  exit 0
fi

AGE_RATIO_PCT=$(awk -v a="$TGT_AGE" -v b="$BASE_AGE" 'BEGIN { if (b == 0) print 0; else printf "%d\n", (a / b) * 100 }')
RSS_RATIO_PCT=$(awk -v a="$TGT_RSS" -v b="$BASE_RSS" 'BEGIN { if (b == 0) print 0; else printf "%d\n", (a / b) * 100 }')

printf "  retention ratio (16/4) = %s %% (ship gate ≥%s %%)\n" "$AGE_RATIO_PCT" "$BENCH_SHIP_RETENTION_PCT"
printf "  rss ratio       (16/4) = %s %% (ship gate ≤%s %%)\n" "$RSS_RATIO_PCT" "$BENCH_SHIP_RSS_PCT"

if (( AGE_RATIO_PCT >= BENCH_SHIP_RETENTION_PCT )) && (( RSS_RATIO_PCT <= BENCH_SHIP_RSS_PCT )); then
  echo "VERDICT: SHIP_16MIB (retention gain ≥${BENCH_SHIP_RETENTION_PCT}% with ≤${BENCH_SHIP_RSS_PCT}% rss; flip ringbuf.DefaultBytesBudget to 16 MiB)"
else
  echo "VERDICT: HOLD_DEFAULT (gate not crossed; keep 4 MiB default — see docs/site/content/architecture/benchmarks/otel-ringbuf.md)"
fi
