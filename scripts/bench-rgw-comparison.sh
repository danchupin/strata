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
#   bash scripts/bench-rgw-comparison.sh --preflight
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
#   --preflight       run pre-flight checks only and exit (US-003)
#   --skip-preflight  skip pre-flight checks (for dry-run / dev iteration)
#
# Env:
#   STRATA_ENDPOINT_URL          default http://localhost:9999
#   STRATA_STATIC_CREDENTIALS    access:secret[,...] — first pair used (matches existing bench scripts)
#   RGW_ENDPOINT_URL             default http://localhost:9991
#   RGW_BENCH_CREDS_FILE         default ./rgw-creds.env — `access_key=...\nsecret_key=...` (written
#                                by US-001 rgw-entrypoint.sh inside the rgw container at
#                                /etc/strata-bench/rgw-creds.env; extract via --extract-rgw-creds).
#   BENCH_RESULTS_DIR            default scripts/bench-results
#   BENCH_TOOL_PATH              override warp binary path (default: $HOME/go/bin/warp)
#   REQUIRE_LAB                  if 1, lab-missing aborts hard instead of EXIT 77
#   STRATA_BENCH_SINGLE_CLUSTER  if 1, restart strata-a/b with single RADOS cluster
#                                (default:/etc/ceph-a/...) to match RGW's cluster count for
#                                fair comparison (US-003). Multi-cluster benchmarking parked.
#   STRATA_BENCH_MIN_DISK_GB     pre-flight free-disk floor (default 300; multipart × 2
#                                targets × 3 runs needs ~300GB transient).
#   STRATA_BENCH_MAX_MEM_GB      pre-flight docker-mem ceiling (default 6; 8GB lima default
#                                leaves 2GB headroom for warp).
#   STRATA_BENCH_DATA_BACKEND_EXPECT  override expected /readyz data_backend (default "rados";
#                                set to "any" to skip the check — useful for memory-backend dev runs).
#   BENCH_COMPOSE_FILE           default deploy/docker/docker-compose.yml
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
STRATA_BENCH_SINGLE_CLUSTER="${STRATA_BENCH_SINGLE_CLUSTER:-0}"
STRATA_BENCH_MIN_DISK_GB="${STRATA_BENCH_MIN_DISK_GB:-300}"
STRATA_BENCH_MAX_MEM_GB="${STRATA_BENCH_MAX_MEM_GB:-6}"
STRATA_BENCH_DATA_BACKEND_EXPECT="${STRATA_BENCH_DATA_BACKEND_EXPECT:-rados}"
BENCH_COMPOSE_FILE="${BENCH_COMPOSE_FILE:-deploy/docker/docker-compose.yml}"
SKIP_PREFLIGHT="${SKIP_PREFLIGHT:-0}"
SINGLE_CLUSTER_ENV="default:/etc/ceph-a/ceph.conf:/etc/ceph-a/ceph.client.admin.keyring"

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
# Pre-flight checks (US-003)
#
# Run before any workload. Catches lab in a bad state up-front rather than
# letting the bench waste 2 hours producing garbage. Six gates:
#
#   (a) strata /readyz returns 200
#   (b) strata data_backend == STRATA_BENCH_DATA_BACKEND_EXPECT ("rados" default).
#       /readyz returns plain-text "ok" today (no JSON shape with data_backend
#       field) so this gate falls back to grep'ing `docker compose logs
#       strata-a` for the `"data":"<backend>"` field emitted on the
#       "strata server listening" startup line. Chosen path: docker-logs grep.
#       Reasoning: pure bench cycle, no Go code changes needed; /readyz body
#       change would touch internal/health + every consumer. Caveat: requires
#       docker compose available + recent enough log retention; warns and
#       proceeds if logs gone. Override expectation via
#       STRATA_BENCH_DATA_BACKEND_EXPECT=any.
#   (c) rgw responds on / (200 or 403 — squid returns 200 with empty
#       ListAllMyBucketsResult; older RGWs return 403 anonymous)
#   (d) host free disk >= STRATA_BENCH_MIN_DISK_GB (default 300; multipart ×
#       2 targets × 3 runs needs ~300GB transient)
#   (e) total docker container memory <= STRATA_BENCH_MAX_MEM_GB (default 6;
#       8GB lima default leaves 2GB headroom for warp)
#   (f) STRATA_RADOS_CLUSTERS value logged so the report doc can reference
#       actual lab config
# -----------------------------------------------------------------------------

compose_cmd() {
  printf 'docker compose -f %s' "$BENCH_COMPOSE_FILE"
}

# Best-effort host free-disk in GB. Tries /var/lib/docker (Linux native), then
# /, then $HOME. macOS + lima/colima typically mount the docker root inside a
# VM not visible from the host — `df $HOME` captures host free disk available
# to the lima VM via 9p/virtiofs.
disk_free_gb() {
  local candidates=("/var/lib/docker" "/" "$HOME") path avail_kb
  for path in "${candidates[@]}"; do
    [[ -d "$path" ]] || continue
    avail_kb="$(df -Pk "$path" 2>/dev/null | awk 'NR==2 { print $4 }')"
    if [[ -n "$avail_kb" && "$avail_kb" =~ ^[0-9]+$ ]]; then
      awk -v kb="$avail_kb" 'BEGIN { printf("%d", kb/1024/1024) }'
      return 0
    fi
  done
  echo 0
}

# Total docker container memory usage in GB (sum of MEM USAGE first column).
docker_mem_used_gb() {
  if ! command -v docker >/dev/null 2>&1; then
    echo 0; return 0
  fi
  docker stats --no-stream --format '{{.MemUsage}}' 2>/dev/null \
    | awk '
        function to_gib(v, u) {
          if (u ~ /^Ki?B?$/) return v/1024/1024
          if (u ~ /^Mi?B?$/) return v/1024
          if (u ~ /^Gi?B?$/) return v
          if (u ~ /^Ti?B?$/) return v*1024
          return 0
        }
        {
          # Field 1 is "<value><unit>" e.g. "123.4MiB"; split into number+unit.
          n = $1
          # match leading float, capture unit suffix
          if (match(n, /^[0-9.]+/)) {
            val = substr(n, RSTART, RLENGTH) + 0
            unit = substr(n, RSTART+RLENGTH)
          } else { val = 0; unit = "" }
          total += to_gib(val, unit)
        }
        END { printf("%.2f", total+0) }
      '
}

# Grep `docker compose logs strata-a` for the startup `"data":"<backend>"`
# field. Emits backend name on stdout, empty if not found.
strata_data_backend_from_logs() {
  local svc="${1:-strata-a}"
  local logs
  logs="$($(compose_cmd) logs --tail=500 "$svc" 2>/dev/null)" || true
  printf '%s\n' "$logs" \
    | grep -m1 -oE '"data":"[a-z0-9]+"' \
    | head -1 \
    | sed -E 's/.*"data":"([a-z0-9]+)".*/\1/'
}

# Grep STRATA_RADOS_CLUSTERS env from running container (defensive — the env
# value at boot time is the source of truth for the bench, not the compose
# file default). Falls back to compose file default if container missing.
strata_rados_clusters_from_container() {
  local svc="${1:-strata-a}"
  local v
  v="$($(compose_cmd) exec -T "$svc" sh -c 'printf %s "$STRATA_RADOS_CLUSTERS"' 2>/dev/null)" || true
  printf '%s' "$v"
}

preflight_check() {
  local target="$1"
  note "pre-flight checks (US-003)"

  # (a) + (b) strata
  if [[ "$target" == "strata" || "$target" == "both" ]]; then
    local code
    code="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 3 "$STRATA_ENDPOINT_URL/readyz" 2>/dev/null || true)"
    if [[ "$code" != "200" ]]; then
      fail "pre-flight: strata /readyz returned HTTP $code (expected 200). Bring lab up: make up-all && make wait-strata-lab"
    fi
    note "  [a] strata /readyz: 200 OK"

    if [[ "$STRATA_BENCH_DATA_BACKEND_EXPECT" == "any" ]]; then
      note "  [b] strata data_backend check skipped (STRATA_BENCH_DATA_BACKEND_EXPECT=any)"
    else
      local backend
      backend="$(strata_data_backend_from_logs strata-a)"
      if [[ -z "$backend" ]]; then
        # Logs may have rotated past the startup line — WARN, do not fail.
        note "  [b] strata data_backend: UNKNOWN (docker compose logs strata-a grep missed startup line — log rotation? proceeding with WARN)"
      elif [[ "$backend" != "$STRATA_BENCH_DATA_BACKEND_EXPECT" ]]; then
        fail "pre-flight: strata data_backend=$backend, expected '$STRATA_BENCH_DATA_BACKEND_EXPECT'. Bench meaningless on $backend backend. Override via STRATA_BENCH_DATA_BACKEND_EXPECT=any to skip."
      else
        note "  [b] strata data_backend: $backend (matches expected '$STRATA_BENCH_DATA_BACKEND_EXPECT')"
      fi
    fi
  fi

  # (c) rgw
  if [[ "$target" == "rgw" || "$target" == "both" ]]; then
    local code
    code="$(probe_target "$RGW_ENDPOINT_URL")"
    if [[ "$code" != "200" && "$code" != "403" ]]; then
      fail "pre-flight: rgw $RGW_ENDPOINT_URL/ returned HTTP $code (expected 200 or 403). Bring lab up: make up-bench-rgw && make wait-rgw"
    fi
    note "  [c] rgw $RGW_ENDPOINT_URL: HTTP $code OK"
  fi

  # (d) disk
  local free_gb
  free_gb="$(disk_free_gb)"
  if (( free_gb < STRATA_BENCH_MIN_DISK_GB )); then
    fail "pre-flight: free disk ${free_gb}GB < required ${STRATA_BENCH_MIN_DISK_GB}GB. Multipart × 2 targets × 3 runs needs ~300GB transient. Free space or override STRATA_BENCH_MIN_DISK_GB."
  fi
  note "  [d] free disk: ${free_gb}GB (>= ${STRATA_BENCH_MIN_DISK_GB}GB required)"

  # (e) mem
  local used_gb
  used_gb="$(docker_mem_used_gb)"
  if awk -v u="$used_gb" -v m="$STRATA_BENCH_MAX_MEM_GB" 'BEGIN { exit !(u+0 > m+0) }'; then
    fail "pre-flight: docker container memory used ${used_gb}GB > ceiling ${STRATA_BENCH_MAX_MEM_GB}GB. warp + lab will OOM on 8GB lima default. Stop unused services or override STRATA_BENCH_MAX_MEM_GB."
  fi
  note "  [e] docker mem: ${used_gb}GB used (<= ${STRATA_BENCH_MAX_MEM_GB}GB ceiling)"

  # (f) reflected RADOS_CLUSTERS
  local rados_clusters
  rados_clusters="$(strata_rados_clusters_from_container strata-a)"
  if [[ -z "$rados_clusters" ]]; then
    note "  [f] STRATA_RADOS_CLUSTERS: <unable to read — strata-a not exec-able; methodology will note compose default>"
  else
    note "  [f] STRATA_RADOS_CLUSTERS=$rados_clusters"
  fi
}

# -----------------------------------------------------------------------------
# Single-cluster bench mode (US-003)
#
# Lab default is multi-cluster RADOS (default + cephb). RGW connects only to
# ceph-a so a fair Strata-vs-RGW comparison must reduce Strata to one cluster
# too. STRATA_BENCH_SINGLE_CLUSTER=1 restarts strata-a + strata-b with
# STRATA_RADOS_CLUSTERS=default:... only. The compose env var is already
# overridable (deploy/docker/docker-compose.yml uses ${STRATA_RADOS_CLUSTERS:-...}).
#
# After restart, polls /readyz until 200 (cap 60s) so the bench doesn't run
# against half-initialized replicas.
# -----------------------------------------------------------------------------

apply_single_cluster_mode() {
  [[ "$STRATA_BENCH_SINGLE_CLUSTER" == "1" ]] || return 0
  note "single-cluster bench mode: restarting strata-a/b with STRATA_RADOS_CLUSTERS=$SINGLE_CLUSTER_ENV"
  STRATA_RADOS_CLUSTERS="$SINGLE_CLUSTER_ENV" $(compose_cmd) up -d strata-a strata-b >&2 \
    || abort "single-cluster mode: docker compose up -d strata-a strata-b failed"

  local deadline=$(( SECONDS + 60 ))
  while (( SECONDS < deadline )); do
    local code
    code="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 2 "$STRATA_ENDPOINT_URL/readyz" 2>/dev/null || true)"
    if [[ "$code" == "200" ]]; then
      note "single-cluster mode: strata /readyz 200 OK"
      return 0
    fi
    sleep 1
  done
  fail "single-cluster mode: strata /readyz did not return 200 within 60s after restart"
}

# -----------------------------------------------------------------------------
# Per-workload cleanup discipline (US-003)
#
# Runs after each (workload, target) cycle. Drops the workload bucket;
# best-effort polls `ceph df` (capped at 30s) to surface async chunk-GC
# progress so methodology can document the limitation. Strata's GC worker
# runs on STRATA_GC_INTERVAL (default 5m) — 30s will NOT see full recovery;
# we log the snapshot for documentation rather than gate on it.
# -----------------------------------------------------------------------------

cleanup_workload() {
  local target="$1" workload="$2"
  local endpoint creds_ak creds_sk bucket
  case "$target" in
    strata) endpoint="$STRATA_ENDPOINT_URL"; creds_ak="$STRATA_AK"; creds_sk="$STRATA_SK" ;;
    rgw)    endpoint="$RGW_ENDPOINT_URL";    creds_ak="$RGW_AK";    creds_sk="$RGW_SK" ;;
    *) note "cleanup_workload: unknown target $target — skip"; return 0 ;;
  esac
  bucket="bench-${workload}-${target}"

  note "cleanup: drop bucket $bucket on $target"
  AWS_ACCESS_KEY_ID="$creds_ak" AWS_SECRET_ACCESS_KEY="$creds_sk" AWS_DEFAULT_REGION=us-east-1 \
    aws --endpoint-url "$endpoint" s3 rm "s3://$bucket" --recursive >/dev/null 2>&1 || true
  AWS_ACCESS_KEY_ID="$creds_ak" AWS_SECRET_ACCESS_KEY="$creds_sk" AWS_DEFAULT_REGION=us-east-1 \
    aws --endpoint-url "$endpoint" s3api delete-bucket --bucket "$bucket" >/dev/null 2>&1 || true

  # Best-effort ceph df snapshot for methodology (cap 30s).
  local snap_deadline=$(( SECONDS + 30 ))
  while (( SECONDS < snap_deadline )); do
    if $(compose_cmd) exec -T ceph ceph df --format json 2>/dev/null \
        | grep -oE '"total_bytes":[0-9]+' | head -1 >/dev/null 2>&1; then
      break
    fi
    sleep 2
  done
  local ceph_df_summary
  ceph_df_summary="$($(compose_cmd) exec -T ceph ceph df 2>/dev/null | head -3 | tail -2 | tr '\n' '|' || true)"
  [[ -n "$ceph_df_summary" ]] && note "  ceph df (post-cleanup snapshot): $ceph_df_summary"
}

# Sanity assertion between targets: before running on target B, verify the
# workload bucket on target A is gone (cleanup_workload above ran best-effort
# delete-bucket; this catches drift if rm or delete-bucket silently failed).
assert_bucket_absent() {
  local target="$1" workload="$2"
  local endpoint creds_ak creds_sk
  case "$target" in
    strata) endpoint="$STRATA_ENDPOINT_URL"; creds_ak="$STRATA_AK"; creds_sk="$STRATA_SK" ;;
    rgw)    endpoint="$RGW_ENDPOINT_URL";    creds_ak="$RGW_AK";    creds_sk="$RGW_SK" ;;
    *) return 0 ;;
  esac
  local bucket="bench-${workload}-${target}"
  local code
  code="$(AWS_ACCESS_KEY_ID="$creds_ak" AWS_SECRET_ACCESS_KEY="$creds_sk" AWS_DEFAULT_REGION=us-east-1 \
    aws --endpoint-url "$endpoint" s3api head-bucket --bucket "$bucket" 2>&1 | head -1)"
  if [[ "$code" != *"Not Found"* && "$code" != *"NoSuchBucket"* && "$code" != *"404"* && -n "$code" ]]; then
    # head-bucket returned no error → bucket still exists. Hard-fail to surface drift.
    fail "sanity: bucket $bucket on $target still exists after cleanup_workload (head-bucket output: $code)"
  fi
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

  cleanup_workload "$target" "$workload"
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
    --preflight) MODE="preflight" ;;
    --skip-preflight) SKIP_PREFLIGHT=1 ;;
    -h|--help) sed -n '1,55p' "$0"; exit 0 ;;
    --) shift; break ;;
    -*) abort "unknown flag: $1" ;;
    *) POSITIONAL+=("$1") ;;
  esac
  shift
done

case "$MODE" in
  report)
    ensure_tool
    do_report
    exit 0
    ;;
  extract-rgw-creds)
    do_extract_rgw_creds
    exit 0
    ;;
  preflight)
    TARGET="${POSITIONAL[0]:-both}"
    extract_strata_creds
    [[ "$TARGET" == "rgw" || "$TARGET" == "both" ]] && extract_rgw_creds
    preflight_check "$TARGET"
    note "pre-flight OK"
    exit 0
    ;;
  run)
    [[ "${#POSITIONAL[@]}" -lt 2 ]] && abort "need: <workload> <target>"
    WORKLOAD="${POSITIONAL[0]}"
    TARGET="${POSITIONAL[1]}"
    ;;
esac

ensure_tool
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

# Single-cluster mode (US-003) — flip BEFORE preflight so the data_backend
# grep covers the restarted strata-a logs.
apply_single_cluster_mode

if [[ "$SKIP_PREFLIGHT" != "1" ]]; then
  preflight_check "$TARGET"
else
  note "pre-flight checks skipped (SKIP_PREFLIGHT=1 / --skip-preflight)"
fi

note "results file: $RESULTS_FILE"

case "$TARGET" in
  strata|rgw)
    dispatch_workload "$WORKLOAD" "$TARGET" "$CONCURRENCY" "$RUNS"
    ;;
  both)
    dispatch_workload "$WORKLOAD" "strata" "$CONCURRENCY" "$RUNS"
    assert_bucket_absent "strata" "$WORKLOAD"
    dispatch_workload "$WORKLOAD" "rgw" "$CONCURRENCY" "$RUNS"
    assert_bucket_absent "rgw" "$WORKLOAD"
    ;;
  *)
    abort "target must be one of: strata, rgw, both (got '$TARGET')"
    ;;
esac

note "done — see $RESULTS_FILE (run with --report for markdown aggregate)"
