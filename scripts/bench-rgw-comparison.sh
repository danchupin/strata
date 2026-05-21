#!/usr/bin/env bash
# Strata vs RGW comparison bench (US-002 of ralph/rgw-benchmarks).
#
# Drives minio/warp against Strata (TiKV-default lab) and a side-by-side
# RGW container (deploy/docker/docker-compose.yml `bench-rgw` profile) so
# operators can validate the README "drop-in RGW replacement" claim.
#
# This story (US-002) ships the scaffold + reference workload `put-small`
# (1KiB PUT × 60s × 8 concurrent × 3 runs). Subsequent stories US-004..US-008
# extend with concurrency sweeps + larger object sizes + multipart + list +
# range/delete/IAM workloads on top of the same scaffold.
#
# Usage:
#   bash scripts/bench-rgw-comparison.sh <workload> <target> [--concurrency=N] [--runs=N]
#   bash scripts/bench-rgw-comparison.sh --report
#   bash scripts/bench-rgw-comparison.sh --extract-rgw-creds
#
# Args:
#   <workload>  one of: put-small (US-002 reference). Others added in US-004+.
#   <target>    one of: strata, rgw, both
#
# Flags:
#   --concurrency=N   override default concurrency for the workload (default 8 for put-small)
#   --runs=N          number of repeated runs (default 3)
#   --report          aggregate jsonl into markdown table on stdout (no run)
#   --extract-rgw-creds   helper: `docker compose exec rgw cat /etc/strata-bench/rgw-creds.env`
#                          pipes the bench user access/secret to ./rgw-creds.env on the host
#
# Env:
#   STRATA_ENDPOINT_URL       default http://localhost:9999
#   STRATA_STATIC_CREDENTIALS access:secret[,...] — first pair used (matches existing bench scripts)
#   RGW_ENDPOINT_URL          default http://localhost:9991
#   RGW_BENCH_CREDS_FILE      default ./rgw-creds.env — `access_key=...\nsecret_key=...` (written
#                             by US-001 rgw-entrypoint.sh inside the rgw container at
#                             /etc/strata-bench/rgw-creds.env; extract via --extract-rgw-creds).
#   BENCH_RESULTS_DIR         default scripts/bench-results
#   BENCH_TOOL_PATH           override warp binary path (default: $HOME/go/bin/warp)
#   REQUIRE_LAB               if 1, lab-missing aborts hard instead of EXIT 77
#
# Exit codes: 0 success, 2 misconfig, 77 lab not reachable (skip).

set -euo pipefail

# -----------------------------------------------------------------------------
# Config + tool discovery
# -----------------------------------------------------------------------------

STRATA_ENDPOINT_URL="${STRATA_ENDPOINT_URL:-http://localhost:9999}"
RGW_ENDPOINT_URL="${RGW_ENDPOINT_URL:-http://localhost:9991}"
RGW_BENCH_CREDS_FILE="${RGW_BENCH_CREDS_FILE:-./rgw-creds.env}"
BENCH_RESULTS_DIR="${BENCH_RESULTS_DIR:-scripts/bench-results}"
BENCH_TOOL_PATH="${BENCH_TOOL_PATH:-$HOME/go/bin/warp}"
REQUIRE_LAB="${REQUIRE_LAB:-0}"

DATE_TAG="$(date +%Y-%m-%d)"
RESULTS_FILE="${BENCH_RESULTS_DIR}/rgw-comparison-${DATE_TAG}.jsonl"

fail()  { echo "FAIL: $*" >&2; exit 1; }
abort() { echo "ABORT: $*" >&2; exit 2; }
skip()  { echo "SKIP: $*" >&2; exit 77; }
note()  { echo "INFO: $*" >&2; }

ensure_tool() {
  if [[ ! -x "$BENCH_TOOL_PATH" ]]; then
    note "warp not at $BENCH_TOOL_PATH — installing"
    go install github.com/minio/warp@latest 2>&1 | tail -5 >&2 \
      || abort "warp install failed. Manual: go install github.com/minio/warp@latest"
  fi
  if [[ ! -x "$BENCH_TOOL_PATH" ]]; then
    abort "warp still missing after install (path=$BENCH_TOOL_PATH)"
  fi
  local ver
  ver="$($BENCH_TOOL_PATH --version 2>&1 | head -1)"
  note "bench tool: $ver"
  note "tool decision (US-002): warp chosen — wasabi-tech/s3-bench repo does not exist"
  note "                       on github (404). Warp supports put/get/multipart/list/delete."
}

extract_strata_creds() {
  local cred="${STRATA_STATIC_CREDENTIALS:-}"
  if [[ -z "$cred" ]]; then abort "STRATA_STATIC_CREDENTIALS unset"; fi
  local first="${cred%%,*}"
  STRATA_AK="${first%%:*}"
  local rest="${first#*:}"
  STRATA_SK="${rest%%:*}"
  if [[ -z "$STRATA_AK" || -z "$STRATA_SK" || "$STRATA_AK" == "$first" ]]; then
    abort "cannot parse STRATA_STATIC_CREDENTIALS='$first' (need access:secret)"
  fi
  return 0
}

extract_rgw_creds() {
  if [[ ! -f "$RGW_BENCH_CREDS_FILE" ]]; then
    abort "RGW creds file '$RGW_BENCH_CREDS_FILE' missing. Run: bash $0 --extract-rgw-creds"
  fi
  RGW_AK="$(grep -E '^access_key=' "$RGW_BENCH_CREDS_FILE" | head -1 | cut -d= -f2-)"
  RGW_SK="$(grep -E '^secret_key=' "$RGW_BENCH_CREDS_FILE" | head -1 | cut -d= -f2-)"
  if [[ -z "$RGW_AK" || -z "$RGW_SK" ]]; then
    abort "could not parse access_key/secret_key from $RGW_BENCH_CREDS_FILE"
  fi
  return 0
}

# Pull rgw bench user creds from the running rgw container.
do_extract_rgw_creds() {
  local compose_cmd="${COMPOSE_CMD:-docker compose -f deploy/docker/docker-compose.yml}"
  $compose_cmd exec -T rgw cat /etc/strata-bench/rgw-creds.env > "$RGW_BENCH_CREDS_FILE" \
    || abort "docker compose exec rgw cat creds failed (is bench-rgw profile up?)"
  note "wrote $RGW_BENCH_CREDS_FILE ($(wc -l < "$RGW_BENCH_CREDS_FILE") lines)"
}

probe_target() {
  local url="$1"
  curl -sS -o /dev/null -w '%{http_code}' --max-time 3 "$url/" 2>/dev/null || true
}

# Strip scheme from endpoint URL (warp wants host:port, not http://host:port).
url_host() {
  local u="$1"
  u="${u#http://}"
  u="${u#https://}"
  u="${u%/}"
  printf '%s' "$u"
}

# -----------------------------------------------------------------------------
# JSONL emit + parse warp text summary
# -----------------------------------------------------------------------------

emit_row() {
  # JSON line: target, workload, object_size, concurrency, run_id,
  #   p50_ms, p95_ms, p99_ms, throughput_mbps, throughput_ops_per_sec,
  #   errors, duration_sec, ts, tool_version
  local target="$1" workload="$2" object_size="$3" concurrency="$4" run_id="$5"
  local p50_ms="$6" p95_ms="$7" p99_ms="$8"
  local throughput_mbps="$9" throughput_ops="${10}"
  local errors="${11}" duration_sec="${12}"
  local ts tool_version
  ts="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  tool_version="$($BENCH_TOOL_PATH --version 2>&1 | head -1 | tr -d '"\n')"

  mkdir -p "$BENCH_RESULTS_DIR"
  printf '{"target":"%s","workload":"%s","object_size":"%s","concurrency":%d,"run_id":%d,"p50_ms":%s,"p95_ms":%s,"p99_ms":%s,"throughput_mbps":%s,"throughput_ops_per_sec":%s,"errors":%d,"duration_sec":%d,"ts":"%s","tool":"warp","tool_version":"%s"}\n' \
    "$target" "$workload" "$object_size" "$concurrency" "$run_id" \
    "$p50_ms" "$p95_ms" "$p99_ms" "$throughput_mbps" "$throughput_ops" \
    "$errors" "$duration_sec" "$ts" "$tool_version" \
    | tee -a "$RESULTS_FILE"
}

# Parse warp's stdout text summary. Warp prints (verified against
# v(dev) 2026-05-21):
#   Report: PUT. Concurrency: 8. Ran: 7s
#    * Average: 0.22 MiB/s, 222.08 obj/s
#    * Reqs: Avg: 39.8ms, 50%: 11.6ms, 90%: 143.0ms, 99%: 212.2ms, Fastest: ...
# We extract p50_ms / p99_ms / throughput_mbps / throughput_ops_per_sec.
# Warp does NOT emit p95 in the text summary (only 50/90/99); we report
# p95 as the linear interpolant between p50 and p99 — documented
# approximation, refined in US-004 via raw csv.zst export if needed.
# Warp's `--analyze.out=FILE` does NOT write FILE; the summary is on
# stdout only. Script captures stdout to summary_file then parses here.
parse_warp_summary() {
  # Disable set -e / pipefail inside parser — grep -m1 -oE returns 1 on
  # missing match (e.g. warp text omits Errors: line when zero errors),
  # which would otherwise tear down the calling subshell mid-parse and
  # silently zero-out all fields.
  set +eo pipefail
  local file="$1" op="$2"

  # Pull the "Report: <op>." block (6-line cluster starting at Report).
  local block
  block="$(awk -v op="$op" '
    BEGIN { in_block=0; lines=0 }
    $0 ~ "^Report: "op"\\." { in_block=1; lines=0 }
    in_block { print; lines++ }
    in_block && lines >= 6 { exit }
  ' "$file" 2>/dev/null)"

  local mbps ops_per_sec p50_ms p99_ms errors
  mbps="$(printf '%s' "$block" | grep -m1 -oE 'Average: [0-9.]+\s*[KMG]?iB/s' \
    | awk '{ v=$2; u=$3; if (u ~ /KiB/) v=v/1024; else if (u ~ /GiB/) v=v*1024; printf("%.3f", v) }')"
  ops_per_sec="$(printf '%s' "$block" | grep -m1 -oE '[0-9.]+ obj/s' | awk '{print $1}')"
  p50_ms="$(printf '%s' "$block" | grep -m1 -oE '50%: [0-9.]+m?s' \
    | awk '{ v=$2; if (v ~ /ms/) { gsub(/ms/,"",v) } else if (v ~ /s/) { gsub(/s/,"",v); v=v*1000 }; printf("%.3f", v) }')"
  p99_ms="$(printf '%s' "$block" | grep -m1 -oE '99%: [0-9.]+m?s' \
    | awk '{ v=$2; if (v ~ /ms/) { gsub(/ms/,"",v) } else if (v ~ /s/) { gsub(/s/,"",v); v=v*1000 }; printf("%.3f", v) }')"
  errors="$(printf '%s' "$block" | grep -m1 -oE 'Errors: [0-9]+' | awk '{print $2}')"

  : "${mbps:=0.0}"
  : "${ops_per_sec:=0.0}"
  : "${p50_ms:=0.0}"
  : "${p99_ms:=0.0}"
  : "${errors:=0}"

  # Linear-interpolant p95 (documented approximation — see comment above).
  local p95_ms
  p95_ms="$(awk -v p50="$p50_ms" -v p99="$p99_ms" 'BEGIN { printf("%.3f", p50 + (p99 - p50) * 0.9184) }')"

  printf '%s %s %s %s %s %s\n' "$p50_ms" "$p95_ms" "$p99_ms" "$mbps" "$ops_per_sec" "$errors"
}

# -----------------------------------------------------------------------------
# Workload runners (warp invocations)
# -----------------------------------------------------------------------------

# Single put workload: 1KiB × duration × concurrency × runs.
# Args: target workload object_size_label concurrency duration runs
workload_put() {
  local target="$1" workload="$2" size_label="$3" concurrency="$4" duration="$5" runs="$6"
  local endpoint creds_ak creds_sk
  case "$target" in
    strata) endpoint="$STRATA_ENDPOINT_URL"; creds_ak="$STRATA_AK"; creds_sk="$STRATA_SK" ;;
    rgw)    endpoint="$RGW_ENDPOINT_URL";    creds_ak="$RGW_AK";    creds_sk="$RGW_SK" ;;
    *) abort "unknown target: $target" ;;
  esac
  local host
  host="$(url_host "$endpoint")"

  local bucket="bench-${workload}-${target}"
  local tmpdir
  tmpdir="$(mktemp -d)"
  trap "rm -rf $tmpdir" RETURN

  note "creating bucket $bucket on $target ($endpoint)"
  AWS_ACCESS_KEY_ID="$creds_ak" AWS_SECRET_ACCESS_KEY="$creds_sk" AWS_DEFAULT_REGION=us-east-1 \
    aws --endpoint-url "$endpoint" s3api create-bucket --bucket "$bucket" >/dev/null 2>&1 || true

  for run in $(seq 1 "$runs"); do
    local summary_file="$tmpdir/warp-$target-$workload-run$run.txt"
    local benchdata_file="$tmpdir/warp-$target-$workload-run$run.bd"
    note "run $run/$runs: warp put $target obj=$size_label conc=$concurrency dur=${duration}s"
    # warp prints summary to stdout (--analyze.out is broken in v(dev)
    # 2026-05-21 — never writes the FILE arg). Capture stdout to
    # summary_file; --benchdata still writes raw csv.zst.json.zst alongside
    # for offline reanalysis.
    set +e
    "$BENCH_TOOL_PATH" put \
      --host="$host" \
      --access-key="$creds_ak" \
      --secret-key="$creds_sk" \
      --bucket="$bucket" \
      --obj.size="$size_label" \
      --concurrent="$concurrency" \
      --duration="${duration}s" \
      --noclear \
      --benchdata="$benchdata_file" \
      --no-color \
      > "$summary_file" 2>&1
    local warp_rc=$?
    set -e
    if [[ "$warp_rc" -ne 0 ]]; then
      note "WARN: warp put failed on $target run $run (rc=$warp_rc) — see $summary_file"
      tail -5 "$summary_file" >&2 || true
      continue
    fi

    read -r p50 p95 p99 mbps ops errs < <(parse_warp_summary "$summary_file" "PUT") || true
    emit_row "$target" "$workload" "$size_label" "$concurrency" "$run" \
      "$p50" "$p95" "$p99" "$mbps" "$ops" "$errs" "$duration"
  done

  note "cleanup bucket $bucket"
  AWS_ACCESS_KEY_ID="$creds_ak" AWS_SECRET_ACCESS_KEY="$creds_sk" AWS_DEFAULT_REGION=us-east-1 \
    aws --endpoint-url "$endpoint" s3 rm "s3://$bucket" --recursive >/dev/null 2>&1 || true
  AWS_ACCESS_KEY_ID="$creds_ak" AWS_SECRET_ACCESS_KEY="$creds_sk" AWS_DEFAULT_REGION=us-east-1 \
    aws --endpoint-url "$endpoint" s3api delete-bucket --bucket "$bucket" >/dev/null 2>&1 || true
}

# -----------------------------------------------------------------------------
# Workload dispatch
# -----------------------------------------------------------------------------

dispatch_workload() {
  local workload="$1" target="$2" concurrency="$3" runs="$4"
  case "$workload" in
    put-small)
      # 1KiB × 60s × 8 concurrent × 3 runs (default — overridable)
      local size_label="${PUT_SMALL_SIZE:-1KiB}"
      local duration="${PUT_SMALL_DURATION:-60}"
      local conc="${concurrency:-8}"
      local r="${runs:-3}"
      workload_put "$target" "put-small" "$size_label" "$conc" "$duration" "$r"
      ;;
    *)
      abort "workload '$workload' not implemented yet (US-002 ships put-small; US-004+ add others)"
      ;;
  esac
}

# -----------------------------------------------------------------------------
# Report aggregator: read jsonl, group by (target, workload, concurrency),
# print mean ± stddev across runs for each metric.
# -----------------------------------------------------------------------------

do_report() {
  [[ -f "$RESULTS_FILE" ]] || { note "no results file: $RESULTS_FILE"; exit 0; }
  command -v jq >/dev/null 2>&1 || abort "jq required for --report"

  echo "# rgw-comparison report ($DATE_TAG)"
  echo ""
  echo "Source: \`$RESULTS_FILE\`"
  echo ""

  # Group by (target, workload, concurrency) — emit mean ± stddev per group.
  local keys
  keys="$(jq -r '"\(.target)|\(.workload)|\(.object_size)|\(.concurrency)"' "$RESULTS_FILE" | sort -u)"

  printf '| target | workload | object_size | concurrency | runs | p50_ms (mean±sd) | p99_ms (mean±sd) | mbps (mean±sd) | ops/s (mean±sd) |\n'
  printf '|--------|----------|-------------|-------------|------|------------------|------------------|----------------|-----------------|\n'
  while IFS='|' read -r tgt wl sz conc; do
    [[ -z "$tgt" ]] && continue
    jq -rs --arg tgt "$tgt" --arg wl "$wl" --arg sz "$sz" --argjson conc "$conc" '
      map(select(.target==$tgt and .workload==$wl and .object_size==$sz and .concurrency==$conc))
      | { runs: length,
          p50: (map(.p50_ms) | add/length),
          p50sd: (map(.p50_ms) as $a | $a | (add/length) as $m | ($a | map(. - $m) | map(. * .) | add / length) | sqrt),
          p99: (map(.p99_ms) | add/length),
          p99sd: (map(.p99_ms) as $a | $a | (add/length) as $m | ($a | map(. - $m) | map(. * .) | add / length) | sqrt),
          mbps: (map(.throughput_mbps) | add/length),
          mbpsd: (map(.throughput_mbps) as $a | $a | (add/length) as $m | ($a | map(. - $m) | map(. * .) | add / length) | sqrt),
          ops: (map(.throughput_ops_per_sec) | add/length),
          opssd: (map(.throughput_ops_per_sec) as $a | $a | (add/length) as $m | ($a | map(. - $m) | map(. * .) | add / length) | sqrt) }
      | "| \($tgt) | \($wl) | \($sz) | \($conc) | \(.runs) | \(.p50|tostring|.[:6])±\(.p50sd|tostring|.[:5]) | \(.p99|tostring|.[:6])±\(.p99sd|tostring|.[:5]) | \(.mbps|tostring|.[:6])±\(.mbpsd|tostring|.[:5]) | \(.ops|tostring|.[:6])±\(.opssd|tostring|.[:5]) |"
    ' "$RESULTS_FILE"
  done <<< "$keys"
}

# -----------------------------------------------------------------------------
# Main
# -----------------------------------------------------------------------------

CONCURRENCY=""
RUNS=""
MODE="run"
POSITIONAL=()

while (($#)); do
  case "$1" in
    --concurrency=*) CONCURRENCY="${1#*=}" ;;
    --runs=*) RUNS="${1#*=}" ;;
    --report) MODE="report" ;;
    --extract-rgw-creds) MODE="extract-rgw-creds" ;;
    -h|--help) sed -n '1,40p' "$0"; exit 0 ;;
    --) shift; break ;;
    -*) abort "unknown flag: $1" ;;
    *) POSITIONAL+=("$1") ;;
  esac
  shift
done

ensure_tool

case "$MODE" in
  report)
    do_report
    exit 0
    ;;
  extract-rgw-creds)
    do_extract_rgw_creds
    exit 0
    ;;
  run)
    [[ "${#POSITIONAL[@]}" -lt 2 ]] && abort "need: <workload> <target>"
    WORKLOAD="${POSITIONAL[0]}"
    TARGET="${POSITIONAL[1]}"
    ;;
esac

extract_strata_creds
[[ "$TARGET" == "rgw" || "$TARGET" == "both" ]] && extract_rgw_creds

# Lab probes — fail-soft via skip(77) unless REQUIRE_LAB=1.
if [[ "$TARGET" == "strata" || "$TARGET" == "both" ]]; then
  code="$(probe_target "$STRATA_ENDPOINT_URL")"
  if [[ "$code" != "200" && "$code" != "403" ]]; then
    if [[ "$REQUIRE_LAB" == "1" ]]; then
      fail "strata endpoint $STRATA_ENDPOINT_URL not ready (HTTP $code). Bring lab up: make up-all && make wait-strata-lab"
    else
      skip "strata endpoint $STRATA_ENDPOINT_URL not ready (HTTP $code). Run 'make up-all' first."
    fi
  fi
fi
if [[ "$TARGET" == "rgw" || "$TARGET" == "both" ]]; then
  code="$(probe_target "$RGW_ENDPOINT_URL")"
  if [[ "$code" != "200" && "$code" != "403" ]]; then
    if [[ "$REQUIRE_LAB" == "1" ]]; then
      fail "rgw endpoint $RGW_ENDPOINT_URL not ready (HTTP $code). Bring lab up: make up-bench-rgw && make wait-rgw"
    else
      skip "rgw endpoint $RGW_ENDPOINT_URL not ready (HTTP $code). Run 'make up-bench-rgw' first."
    fi
  fi
fi

note "results file: $RESULTS_FILE"

case "$TARGET" in
  strata|rgw)
    dispatch_workload "$WORKLOAD" "$TARGET" "$CONCURRENCY" "$RUNS"
    ;;
  both)
    dispatch_workload "$WORKLOAD" "strata" "$CONCURRENCY" "$RUNS"
    dispatch_workload "$WORKLOAD" "rgw" "$CONCURRENCY" "$RUNS"
    ;;
  *)
    abort "target must be one of: strata, rgw, both (got '$TARGET')"
    ;;
esac

note "done — see $RESULTS_FILE (run with --report for markdown aggregate)"
