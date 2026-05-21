#!/usr/bin/env bash
# Strata vs RGW comparison bench (US-002..US-005 of ralph/rgw-benchmarks).
#
# Drives minio/warp against Strata (TiKV-default lab) and a side-by-side
# RGW container (deploy/docker/docker-compose.yml `bench-rgw` profile) so
# operators can validate the README "drop-in RGW replacement" claim.
#
# US-002 shipped the scaffold + reference workload `put-small`. US-004 adds
# put-medium / get-small / get-medium plus a concurrency sweep ({1, 8, 32, 128}
# default) per workload. US-005 adds put-large / get-large (100MiB shape).
# US-006 adds multipart-5g (warp multipart-put: 5 concurrent multipart uploads
# of 5GB each, 80 × 64MiB parts). US-007 adds list (100k 1KiB keys, ListObjectsV2
# max-keys=1000 — the bucket-index claim workload). US-008 adds range-get
# (random 1MiB ranges via warp `get --range-size=1MiB`), delete (warp `delete`
# seeds + DELETEs N small objects), iam-auth (pre-create IAM user via target's
# admin API, then run warp `get` under the IAM user's SigV4 creds).
#
# US-009 adds a 3rd target `cassandra` (Strata + Cassandra meta + same RADOS
# data backend on port 9998 via `make up-cassandra`) plus the `all` target
# value that runs strata + rgw + cassandra in sequence. The `--include-cassandra`
# flag auto-boots the cassandra stack if not already up.
#
# Usage:
#   bash scripts/bench-rgw-comparison.sh <workload> <target> [--concurrency=N | --concurrency-sweep=A,B,C] [--runs=N] [--include-cassandra]
#   bash scripts/bench-rgw-comparison.sh --report
#   bash scripts/bench-rgw-comparison.sh --extract-rgw-creds
#   bash scripts/bench-rgw-comparison.sh --preflight
#
# Args:
#   <workload>  one of:
#                 put-small     1KiB PUT (US-002 reference)
#                 put-medium    1MiB PUT (US-004)
#                 get-small     1KiB GET, warp auto-seeds (US-004)
#                 get-medium    1MiB GET, warp auto-seeds (US-004)
#                 put-large     100MiB PUT, c=4, cleanup between runs (US-005)
#                 get-large     100MiB GET, c=4, warp auto-seeds, cleanup between runs (US-005)
#                 multipart-5g  5 concurrent multipart uploads, 80 × 64MiB parts each
#                                = 5GiB per upload; warp PUTPART op label (US-006)
#                 list          ListObjectsV2 bench against ~100k 1KiB-keyed
#                                bucket (warp's `list` subcommand, op-label
#                                LIST, --max-keys=1000 paginated) — THE
#                                bucket-index claim workload (US-007)
#                 range-get     Random 1MiB-range GETs via warp's `get
#                                --range-size=1MiB`; op-label GET, fixed
#                                range size + random offset (US-008)
#                 delete        warp `delete` — seeds N small objects then
#                                DELETEs them; op-label DELETE (US-008)
#                 iam-auth      warp `get` driven by IAM-user SigV4 creds
#                                created via the target's admin API (Strata
#                                /admin/v1/iam, RGW radosgw-admin); op-label
#                                GET, exercises full SigV4 + identity
#                                resolution hot path (US-008)
#   <target>    one of: strata, rgw, cassandra, both, all
#                 both = strata + rgw (default 2-target shape — US-002..US-008)
#                 all  = strata + rgw + cassandra (US-009; requires --include-cassandra
#                        unless cassandra endpoint already reachable)
#                 cassandra = Strata + Cassandra meta + RADOS data on port 9998
#                             (single target; US-009; requires --include-cassandra
#                              unless cassandra endpoint already reachable)
#
# Flags:
#   --concurrency=N           single concurrency point (default 8). Disables sweep.
#   --concurrency-sweep=A,B,C concurrency sweep points (default "1,8,32,128" for US-004 workloads).
#                              Sweep runs sequentially per workload; bucket reused across conc
#                              points; cleanup_workload runs once at end of workload.
#   --runs=N                  number of repeated runs per (workload, concurrency) point (default 3)
#   --include-cassandra       opt-in: boot strata-cassandra (port 9998) via `make up-cassandra`
#                              if not already up, enable the `cassandra` / `all` target values
#                              (US-009). Default bench remains 2-target (strata + rgw); the
#                              optional 3rd target adds ~50% to full-sweep duration.
#   --report                  aggregate jsonl into markdown tables on stdout (no run).
#                              Emits the flat per-(target,workload,conc) summary table PLUS a
#                              per-workload pivot grid (rows=conc, cols=Strata p99 / RGW p99 /
#                              ratio / combined stddev) for any workload with both targets present.
#                              When cassandra rows are present, an additional "Strata-Cassandra"
#                              column is emitted in the pivot grid (US-009 3-way framing).
#   --extract-rgw-creds       helper: `docker compose exec rgw cat /etc/strata-bench/rgw-creds.env`
#                              pipes the bench user access/secret to ./rgw-creds.env on the host
#   --preflight               run pre-flight checks only and exit (US-003)
#   --skip-preflight          skip pre-flight checks (for dry-run / dev iteration)
#
# Env:
#   STRATA_ENDPOINT_URL          default http://localhost:9999
#   STRATA_STATIC_CREDENTIALS    access:secret[,...] — first pair used (matches existing bench scripts)
#   RGW_ENDPOINT_URL             default http://localhost:9991
#   RGW_BENCH_CREDS_FILE         default ./rgw-creds.env — `access_key=...\nsecret_key=...` (written
#                                by US-001 rgw-entrypoint.sh inside the rgw container at
#                                /etc/strata-bench/rgw-creds.env; extract via --extract-rgw-creds).
#   CASSANDRA_ENDPOINT_URL       default http://localhost:9998 — Strata-Cassandra gateway. Shares
#                                STRATA_STATIC_CREDENTIALS with the TiKV-default lab (compose
#                                env is identical across strata-a/b/strata-cassandra). US-009.
#   BENCH_INCLUDE_CASSANDRA      env equivalent of --include-cassandra (set to 1 to opt-in
#                                without re-passing the flag through Makefile wrappers).
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
#   MULTIPART_5G_PART_SIZE       part size for multipart-5g workload (default 64MiB)
#   MULTIPART_5G_PARTS           parts per multipart upload (default 80 → 5GiB total per upload)
#   MULTIPART_5G_CONCURRENCY     parallel multipart sessions (default 5)
#   MULTIPART_5G_PART_CONCURRENCY in-session part-upload parallelism (default 20;
#                                auto-capped to MULTIPART_5G_PARTS).
#   MULTIPART_5G_DURATION        wall-clock seconds per multipart-5g run (default 60)
#   LIST_SIZE                    object size for list workload seed (default 1KiB)
#   LIST_OBJECTS                 total seeded objects per run for list workload (default
#                                100000; warp distributes across --concurrent workers and
#                                rounds up to equal per-worker count)
#   LIST_CONCURRENCY             concurrent list workers (default 8; PRD's "32 concurrent"
#                                refers to seed-phase PUT concurrency — warp uses the same
#                                flag for seed + measure, so this also governs seed conc)
#   LIST_DURATION                wall-clock seconds per list run (default 60)
#   LIST_MAX_KEYS                ListObjectsV2 max-keys per page (default 1000 per PRD;
#                                with 100k seed × 8 workers = 12500 keys/worker → ~13
#                                pages per LIST op)
#   RANGE_GET_SIZE               base object size for range-get seed (default 100MiB)
#   RANGE_GET_OBJECTS            seeded objects for range-get (default 10 → ~1GiB seed)
#   RANGE_GET_RANGE_SIZE         fixed range size per request (default 1MiB per PRD)
#   RANGE_GET_CONCURRENCY        concurrent range-get workers (default 8)
#   RANGE_GET_DURATION           wall-clock seconds per range-get run (default 60)
#   DELETE_SIZE                  object size for delete seed (default 1KiB)
#   DELETE_OBJECTS               seeded objects per delete run (default 1000 per PRD)
#   DELETE_CONCURRENCY           concurrent delete workers (default 8)
#   DELETE_BATCH                 DELETEs per batch (default auto = objects/(concurrency*4),
#                                min 1; warp enforces objects >= concurrency*batch*4)
#   DELETE_DURATION              wall-clock seconds per delete run (default 60)
#   IAM_AUTH_SIZE                object size for iam-auth seed (default 1KiB)
#   IAM_AUTH_OBJECTS             seeded objects per iam-auth run (default 1000 per PRD)
#   IAM_AUTH_CONCURRENCY         concurrent iam-auth get workers (default 8)
#   IAM_AUTH_DURATION            wall-clock seconds per iam-auth run (default 60)
#   IAM_AUTH_USER                IAM user name created per run (default bench-iam)
#   BENCH_COMPOSE_FILE           default deploy/docker/docker-compose.yml
#
# Exit codes: 0 success, 2 misconfig, 77 lab not reachable (skip).

set -euo pipefail

# -----------------------------------------------------------------------------
# Config + tool discovery
# -----------------------------------------------------------------------------

STRATA_ENDPOINT_URL="${STRATA_ENDPOINT_URL:-http://localhost:9999}"
RGW_ENDPOINT_URL="${RGW_ENDPOINT_URL:-http://localhost:9991}"
CASSANDRA_ENDPOINT_URL="${CASSANDRA_ENDPOINT_URL:-http://localhost:9998}"
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
INCLUDE_CASSANDRA="${BENCH_INCLUDE_CASSANDRA:-0}"

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

  # Per-target gate booleans — `both` = strata+rgw, `all` = strata+rgw+cassandra
  # (US-009). `cassandra` standalone = cassandra-only.
  local want_strata=0 want_rgw=0 want_cassandra=0
  case "$target" in
    strata)    want_strata=1 ;;
    rgw)       want_rgw=1 ;;
    cassandra) want_cassandra=1 ;;
    both)      want_strata=1; want_rgw=1 ;;
    all)       want_strata=1; want_rgw=1; want_cassandra=1 ;;
  esac

  # (a) + (b) strata
  if (( want_strata )); then
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

  # (a') + (b') strata-cassandra (US-009 — same shape as the strata gates).
  if (( want_cassandra )); then
    local code
    code="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 3 "$CASSANDRA_ENDPOINT_URL/readyz" 2>/dev/null || true)"
    if [[ "$code" != "200" ]]; then
      fail "pre-flight: strata-cassandra /readyz returned HTTP $code (expected 200). Bring up: make up-cassandra && make wait-cassandra, or pass --include-cassandra to auto-boot."
    fi
    note "  [a'] strata-cassandra /readyz: 200 OK"

    if [[ "$STRATA_BENCH_DATA_BACKEND_EXPECT" == "any" ]]; then
      note "  [b'] strata-cassandra data_backend check skipped (STRATA_BENCH_DATA_BACKEND_EXPECT=any)"
    else
      local cbackend
      cbackend="$(strata_data_backend_from_logs strata-cassandra)"
      if [[ -z "$cbackend" ]]; then
        note "  [b'] strata-cassandra data_backend: UNKNOWN (docker compose logs strata-cassandra grep missed startup line — proceeding with WARN)"
      elif [[ "$cbackend" != "$STRATA_BENCH_DATA_BACKEND_EXPECT" ]]; then
        fail "pre-flight: strata-cassandra data_backend=$cbackend, expected '$STRATA_BENCH_DATA_BACKEND_EXPECT'. Bench meaningless on $cbackend backend."
      else
        note "  [b'] strata-cassandra data_backend: $cbackend (matches expected '$STRATA_BENCH_DATA_BACKEND_EXPECT')"
      fi
    fi
  fi

  # (c) rgw
  if (( want_rgw )); then
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
  local services="strata-a strata-b"
  # US-009: if cassandra is in play, restart strata-cassandra alongside with
  # the same single-cluster env. Compose's `--profile cassandra` is implied by
  # the service name (compose engine resolves profile-gated services when
  # named directly in `up`).
  if (( INCLUDE_CASSANDRA )); then
    services="$services strata-cassandra"
  fi
  note "single-cluster bench mode: restarting $services with STRATA_RADOS_CLUSTERS=$SINGLE_CLUSTER_ENV"
  # shellcheck disable=SC2086 # intentional word-split on services
  STRATA_RADOS_CLUSTERS="$SINGLE_CLUSTER_ENV" $(compose_cmd) up -d $services >&2 \
    || abort "single-cluster mode: docker compose up -d $services failed"

  local deadline=$(( SECONDS + 60 ))
  while (( SECONDS < deadline )); do
    local code
    code="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 2 "$STRATA_ENDPOINT_URL/readyz" 2>/dev/null || true)"
    if [[ "$code" == "200" ]]; then
      note "single-cluster mode: strata /readyz 200 OK"
      break
    fi
    sleep 1
  done
  if (( INCLUDE_CASSANDRA )); then
    local cdeadline=$(( SECONDS + 60 ))
    while (( SECONDS < cdeadline )); do
      local ccode
      ccode="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 2 "$CASSANDRA_ENDPOINT_URL/readyz" 2>/dev/null || true)"
      if [[ "$ccode" == "200" ]]; then
        note "single-cluster mode: strata-cassandra /readyz 200 OK"
        return 0
      fi
      sleep 1
    done
    fail "single-cluster mode: strata-cassandra /readyz did not return 200 within 60s after restart"
  fi
  return 0
}

# -----------------------------------------------------------------------------
# US-009 cassandra-lab opt-in: boot strata-cassandra (+ underlying cassandra
# service) via `make up-cassandra` if the endpoint isn't already reachable.
# Idempotent — probes first, only invokes make on miss. The 3rd target adds
# ~50% to the full sweep duration; default bench remains 2-target (strata+rgw).
# -----------------------------------------------------------------------------
ensure_cassandra_lab() {
  local code
  code="$(probe_target "$CASSANDRA_ENDPOINT_URL")"
  if [[ "$code" == "200" || "$code" == "403" ]]; then
    note "cassandra lab already up (HTTP $code on $CASSANDRA_ENDPOINT_URL/) — skipping make up-cassandra"
    return 0
  fi
  note "booting strata-cassandra lab via make up-cassandra wait-cassandra wait-ceph (cassandra cold-start ~60s)"
  make up-cassandra wait-cassandra wait-ceph >&2 \
    || abort "make up-cassandra/wait-cassandra/wait-ceph failed (US-009 --include-cassandra)"
  # Wait for strata-cassandra /readyz — wait-cassandra targets the Cassandra
  # DB readiness, not the strata-cassandra gateway. Poll explicitly.
  local deadline=$(( SECONDS + 60 ))
  while (( SECONDS < deadline )); do
    code="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 2 "$CASSANDRA_ENDPOINT_URL/readyz" 2>/dev/null || true)"
    if [[ "$code" == "200" ]]; then
      note "strata-cassandra /readyz 200 OK"
      return 0
    fi
    sleep 2
  done
  abort "strata-cassandra /readyz did not return 200 within 60s of make up-cassandra"
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
    strata)    endpoint="$STRATA_ENDPOINT_URL";    creds_ak="$STRATA_AK"; creds_sk="$STRATA_SK" ;;
    rgw)       endpoint="$RGW_ENDPOINT_URL";       creds_ak="$RGW_AK";    creds_sk="$RGW_SK" ;;
    cassandra) endpoint="$CASSANDRA_ENDPOINT_URL"; creds_ak="$STRATA_AK"; creds_sk="$STRATA_SK" ;;
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
    strata)    endpoint="$STRATA_ENDPOINT_URL";    creds_ak="$STRATA_AK"; creds_sk="$STRATA_SK" ;;
    rgw)       endpoint="$RGW_ENDPOINT_URL";       creds_ak="$RGW_AK";    creds_sk="$RGW_SK" ;;
    cassandra) endpoint="$CASSANDRA_ENDPOINT_URL"; creds_ak="$STRATA_AK"; creds_sk="$STRATA_SK" ;;
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
#
# Factored into bucket lifecycle + inner per-run loops + cleanup. This allows
# a concurrency sweep (US-004) to reuse the same bucket across all conc points
# instead of paying create/teardown per point — PRD specifies "cleanup between
# concurrency points is light (same bucket, just clear keys)". For PUT this
# means keys accumulate across the sweep (warp generates fresh random keys per
# invocation, no overlap). For GET, warp's `get` subcommand auto-seeds the
# bucket on first run and reuses existing objects when --noclear is set.
# -----------------------------------------------------------------------------

# Resolve target → (endpoint, ak, sk) tuple. Sets endpoint/creds_ak/creds_sk
# via nameref to avoid global pollution.
resolve_target() {
  local target="$1"
  local -n _endpoint="$2" _ak="$3" _sk="$4"
  case "$target" in
    strata)    _endpoint="$STRATA_ENDPOINT_URL";    _ak="$STRATA_AK"; _sk="$STRATA_SK" ;;
    rgw)       _endpoint="$RGW_ENDPOINT_URL";       _ak="$RGW_AK";    _sk="$RGW_SK" ;;
    # cassandra target (US-009): Strata-Cassandra shares the same
    # STRATA_STATIC_CREDENTIALS as the TiKV-default lab — single static-creds
    # env propagated to strata-a / strata-b / strata-cassandra via compose
    # (deploy/docker/docker-compose.yml). Only the endpoint changes.
    cassandra) _endpoint="$CASSANDRA_ENDPOINT_URL"; _ak="$STRATA_AK"; _sk="$STRATA_SK" ;;
    *) abort "unknown target: $target" ;;
  esac
}

bucket_create() {
  local target="$1" bucket="$2"
  local endpoint creds_ak creds_sk
  resolve_target "$target" endpoint creds_ak creds_sk
  note "creating bucket $bucket on $target ($endpoint)"
  AWS_ACCESS_KEY_ID="$creds_ak" AWS_SECRET_ACCESS_KEY="$creds_sk" AWS_DEFAULT_REGION=us-east-1 \
    aws --endpoint-url "$endpoint" s3api create-bucket --bucket "$bucket" >/dev/null 2>&1 || true
}

# Single warp put invocation. Caller controls run_id so multi-bucket-per-run
# workloads (US-005 put-large with cleanup-between-runs) keep monotonic run_id
# in the JSONL. Returns non-zero on warp failure so caller can decide whether
# to continue.
# Args: target workload size_label concurrency duration run_id bucket tmpdir
warp_put_one() {
  local target="$1" workload="$2" size_label="$3" concurrency="$4" duration="$5" run="$6" bucket="$7" tmpdir="$8"
  local endpoint creds_ak creds_sk
  resolve_target "$target" endpoint creds_ak creds_sk
  local host
  host="$(url_host "$endpoint")"

  local summary_file="$tmpdir/warp-$target-$workload-c$concurrency-run$run.txt"
  local benchdata_file="$tmpdir/warp-$target-$workload-c$concurrency-run$run.bd"
  note "run $run: warp put $target obj=$size_label conc=$concurrency dur=${duration}s"
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
    return 1
  fi

  read -r p50 p95 p99 mbps ops errs < <(parse_warp_summary "$summary_file" "PUT") || true
  emit_row "$target" "$workload" "$size_label" "$concurrency" "$run" \
    "$p50" "$p95" "$p99" "$mbps" "$ops" "$errs" "$duration"
}

# Inner: N warp put runs against an existing bucket at one concurrency point.
# Args: target workload size_label concurrency duration runs bucket tmpdir
warp_put_runs() {
  local target="$1" workload="$2" size_label="$3" concurrency="$4" duration="$5" runs="$6" bucket="$7" tmpdir="$8"
  local run
  for run in $(seq 1 "$runs"); do
    warp_put_one "$target" "$workload" "$size_label" "$concurrency" "$duration" "$run" "$bucket" "$tmpdir" || true
  done
}

# Single warp get invocation. Caller controls run_id (mirrors warp_put_one).
# warp's `get` subcommand auto-seeds the bucket up to --objects=N (default
# 2500) of size --obj.size, then drains for --duration at --concurrent.
#
# Args: target workload size_label concurrency duration run_id bucket tmpdir seed_objects
warp_get_one() {
  local target="$1" workload="$2" size_label="$3" concurrency="$4" duration="$5" run="$6" bucket="$7" tmpdir="$8" seed_objects="$9"
  local endpoint creds_ak creds_sk
  resolve_target "$target" endpoint creds_ak creds_sk
  local host
  host="$(url_host "$endpoint")"

  local summary_file="$tmpdir/warp-$target-$workload-c$concurrency-run$run.txt"
  local benchdata_file="$tmpdir/warp-$target-$workload-c$concurrency-run$run.bd"
  note "run $run: warp get $target obj=$size_label conc=$concurrency dur=${duration}s seed=$seed_objects"
  set +e
  "$BENCH_TOOL_PATH" get \
    --host="$host" \
    --access-key="$creds_ak" \
    --secret-key="$creds_sk" \
    --bucket="$bucket" \
    --obj.size="$size_label" \
    --objects="$seed_objects" \
    --concurrent="$concurrency" \
    --duration="${duration}s" \
    --noclear \
    --benchdata="$benchdata_file" \
    --no-color \
    > "$summary_file" 2>&1
  local warp_rc=$?
  set -e
  if [[ "$warp_rc" -ne 0 ]]; then
    note "WARN: warp get failed on $target run $run (rc=$warp_rc) — see $summary_file"
    tail -5 "$summary_file" >&2 || true
    return 1
  fi

  # warp get prints both Report: PUT. (prepare phase) and Report: GET.
  # (measurement phase). Parser anchors at start-of-line on "Report: GET."
  # so the prepare-phase PUT block is skipped.
  read -r p50 p95 p99 mbps ops errs < <(parse_warp_summary "$summary_file" "GET") || true
  emit_row "$target" "$workload" "$size_label" "$concurrency" "$run" \
    "$p50" "$p95" "$p99" "$mbps" "$ops" "$errs" "$duration"
}

# Inner: N warp get runs against an existing bucket at one concurrency point.
# With --noclear set inside warp_get_one, the seed is reused across runs /
# conc points (warp tops up only if existing objects insufficient).
#
# Args: target workload size_label concurrency duration runs bucket tmpdir seed_objects
warp_get_runs() {
  local target="$1" workload="$2" size_label="$3" concurrency="$4" duration="$5" runs="$6" bucket="$7" tmpdir="$8" seed_objects="$9"
  local run
  for run in $(seq 1 "$runs"); do
    warp_get_one "$target" "$workload" "$size_label" "$concurrency" "$duration" "$run" "$bucket" "$tmpdir" "$seed_objects" || true
  done
}

# Single-point PUT workload (US-002 shape — retained for backwards compat).
# Args: target workload object_size_label concurrency duration runs
workload_put() {
  local target="$1" workload="$2" size_label="$3" concurrency="$4" duration="$5" runs="$6"
  local bucket="bench-${workload}-${target}"
  local tmpdir
  tmpdir="$(mktemp -d)"
  trap "rm -rf $tmpdir" RETURN

  bucket_create "$target" "$bucket"
  warp_put_runs "$target" "$workload" "$size_label" "$concurrency" "$duration" "$runs" "$bucket" "$tmpdir"
  cleanup_workload "$target" "$workload"
}

# Sweep PUT workload across multiple concurrency points (US-004).
# Args: target workload object_size_label duration runs <conc1 conc2 ...>
workload_put_sweep() {
  local target="$1" workload="$2" size_label="$3" duration="$4" runs="$5"
  shift 5
  local concs=("$@")
  local bucket="bench-${workload}-${target}"
  local tmpdir
  tmpdir="$(mktemp -d)"
  trap "rm -rf $tmpdir" RETURN

  bucket_create "$target" "$bucket"
  note "sweep PUT: $target / $workload / concs=${concs[*]} / dur=${duration}s × runs=$runs"
  local c
  for c in "${concs[@]}"; do
    warp_put_runs "$target" "$workload" "$size_label" "$c" "$duration" "$runs" "$bucket" "$tmpdir"
  done
  cleanup_workload "$target" "$workload"
}

# Sweep GET workload across multiple concurrency points (US-004).
# Args: target workload object_size_label duration runs seed_objects <conc1 conc2 ...>
workload_get_sweep() {
  local target="$1" workload="$2" size_label="$3" duration="$4" runs="$5" seed_objects="$6"
  shift 6
  local concs=("$@")
  local bucket="bench-${workload}-${target}"
  local tmpdir
  tmpdir="$(mktemp -d)"
  trap "rm -rf $tmpdir" RETURN

  bucket_create "$target" "$bucket"
  note "sweep GET: $target / $workload / concs=${concs[*]} / dur=${duration}s × runs=$runs / seed=$seed_objects"
  local c
  for c in "${concs[@]}"; do
    warp_get_runs "$target" "$workload" "$size_label" "$c" "$duration" "$runs" "$bucket" "$tmpdir" "$seed_objects"
  done
  cleanup_workload "$target" "$workload"
}

# Large-object PUT workload (US-005). Single concurrency point per PRD (100MiB
# × c=4 by default — large objects exercise the chunking / manifest / network
# buffer code paths, not the small-object scheduling shape that US-004 sweeps).
# cleanup_workload runs BETWEEN runs (not just at end) — 100MiB × ~50 ops × 4
# concurrent × 3 runs lands ~5GB per run in the bucket; accumulated 15GB+ per
# target without between-run cleanup would burn the ~300GB disk pre-flight
# budget allocated by US-003. Bucket is recreated before each run so warp can
# uniformly emit fresh random keys.
#
# Args: target runs
workload_put_large() {
  local target="$1" runs="$2"
  local size_label="${PUT_LARGE_SIZE:-100MiB}"
  local duration="${PUT_LARGE_DURATION:-60}"
  local concurrency="${PUT_LARGE_CONCURRENCY:-4}"
  local bucket="bench-put-large-${target}"
  local tmpdir; tmpdir="$(mktemp -d)"
  trap "rm -rf $tmpdir" RETURN

  note "workload put-large: $target / $size_label × c=$concurrency × dur=${duration}s × runs=$runs (cleanup between runs)"
  local run
  for run in $(seq 1 "$runs"); do
    bucket_create "$target" "$bucket"
    warp_put_one "$target" "put-large" "$size_label" "$concurrency" "$duration" "$run" "$bucket" "$tmpdir" || true
    cleanup_workload "$target" "put-large"
  done
}

# Large-object GET workload (US-005). Mirrors workload_put_large shape: single
# concurrency point, cleanup between runs. warp's `get` auto-seeds the bucket
# to --objects=N (default 50 = ~5GiB seed for 100MiB shape) on each run's
# prepare phase, then drains for the configured duration.
#
# Args: target runs
workload_get_large() {
  local target="$1" runs="$2"
  local size_label="${GET_LARGE_SIZE:-100MiB}"
  local duration="${GET_LARGE_DURATION:-60}"
  local concurrency="${GET_LARGE_CONCURRENCY:-4}"
  local seed="${GET_LARGE_OBJECTS:-50}"
  local bucket="bench-get-large-${target}"
  local tmpdir; tmpdir="$(mktemp -d)"
  trap "rm -rf $tmpdir" RETURN

  note "workload get-large: $target / $size_label × c=$concurrency × dur=${duration}s × runs=$runs / seed=$seed (cleanup between runs)"
  local run
  for run in $(seq 1 "$runs"); do
    bucket_create "$target" "$bucket"
    warp_get_one "$target" "get-large" "$size_label" "$concurrency" "$duration" "$run" "$bucket" "$tmpdir" "$seed" || true
    cleanup_workload "$target" "get-large"
  done
}

# Single warp multipart-put invocation (US-006). Uses warp's `multipart-put`
# subcommand which spawns `--concurrent` goroutines, each looping:
#   createMultipartUpload → upload `--parts` parts (in `--concurrent` internal
#   slots; one part per part-upload op) → completeMultipartUpload → repeat.
#
# warp emits op-label `PUTPART` per part-upload (pkg/bench/multipart_put.go:130);
# the report block parser anchors on `^Report: PUTPART\.` so the throughput
# / latency stats reflect per-part shape (PRD AC: aggregate throughput MB/s,
# per-part p99 latency).
#
# PRD spec: 5 parallel multipart uploads of 5GiB each, part size 64MiB.
#   --concurrent=5      → 5 simultaneous multipart sessions
#   --parts=80          → 80 × 64MiB = 5120MiB ≈ 5GiB per multipart object
#   --part.size=64MiB
#   --part.concurrent=N → in-session part-upload parallelism (warp's flag,
#                          default 20 in warp; capped to `parts` here so smoke
#                          shapes with smaller --parts don't trip warp's
#                          "part.concurrent can't be more than parts" guard).
# In duration mode (default 60s) warp loops session create→parts→complete so
# multiple multipart objects may complete per concurrent slot. Throughput shape
# (MB/s) absorbs the looping; raw "5 GiB per upload" is the per-object spec
# for the chunking / manifest-size code paths the bench targets.
#
# Args: target workload part_size parts concurrency part_concurrency duration run_id bucket tmpdir
warp_multipart_one() {
  local target="$1" workload="$2" part_size="$3" parts="$4" concurrency="$5" part_concurrency="$6" duration="$7" run="$8" bucket="$9" tmpdir="${10}"
  # warp enforces part.concurrent <= parts; cap defensively.
  if (( part_concurrency > parts )); then
    part_concurrency="$parts"
  fi
  local endpoint creds_ak creds_sk
  resolve_target "$target" endpoint creds_ak creds_sk
  local host
  host="$(url_host "$endpoint")"

  local summary_file="$tmpdir/warp-$target-$workload-c$concurrency-run$run.txt"
  local benchdata_file="$tmpdir/warp-$target-$workload-c$concurrency-run$run.bd"
  # Object label reflects the synthesized per-upload size for operator readability:
  # parts × part.size formatted as a SIZE_BYTES decimal (e.g. 5GiB at 80×64MiB).
  local obj_label
  obj_label="$(awk -v p="$parts" -v ps="$part_size" 'BEGIN {
    n = ps + 0
    if (ps ~ /KiB$/)  mul = 1024
    else if (ps ~ /MiB$/) mul = 1024*1024
    else if (ps ~ /GiB$/) mul = 1024*1024*1024
    else mul = 1
    total = n * mul * p
    if (total >= 1024*1024*1024) printf("%.1fGiB", total/1024/1024/1024)
    else if (total >= 1024*1024) printf("%.0fMiB", total/1024/1024)
    else printf("%dB", total)
  }')"
  note "run $run: warp multipart-put $target part=$part_size × $parts (= $obj_label/upload) conc=$concurrency part.conc=$part_concurrency dur=${duration}s"
  set +e
  "$BENCH_TOOL_PATH" multipart-put \
    --host="$host" \
    --access-key="$creds_ak" \
    --secret-key="$creds_sk" \
    --bucket="$bucket" \
    --part.size="$part_size" \
    --parts="$parts" \
    --concurrent="$concurrency" \
    --part.concurrent="$part_concurrency" \
    --duration="${duration}s" \
    --noclear \
    --benchdata="$benchdata_file" \
    --no-color \
    > "$summary_file" 2>&1
  local warp_rc=$?
  set -e
  if [[ "$warp_rc" -ne 0 ]]; then
    note "WARN: warp multipart-put failed on $target run $run (rc=$warp_rc) — see $summary_file"
    tail -5 "$summary_file" >&2 || true
    return 1
  fi

  read -r p50 p95 p99 mbps ops errs < <(parse_warp_summary "$summary_file" "PUTPART") || true
  # object_size column in JSONL carries the per-part size (latency / throughput
  # rows in the report are per-part); the per-upload object size lives in the
  # workload label `multipart-5g` (= 80 × 64MiB synth).
  emit_row "$target" "$workload" "$part_size" "$concurrency" "$run" \
    "$p50" "$p95" "$p99" "$mbps" "$ops" "$errs" "$duration"
}

# Multipart workload (US-006). PRD: 5 parallel multipart uploads of 5GB each,
# part size 64MB. Maps to warp's `multipart-put` subcommand: --concurrent=5
# parallel multipart sessions, each session uploads --parts=80 parts of
# --part.size=64MiB (= 5120MiB ≈ 5GiB per upload). Cleanup between runs is
# mandatory — 25GiB per run × 3 runs would otherwise burn ~75GiB before drop;
# with cleanup-between-runs the peak per-run transient stays at single-run
# size (~5GiB × concurrent slots ≈ 25GiB). Bucket recreated per run so warp's
# random object names don't accumulate stale uploads.
#
# Args: target runs
workload_multipart_5g() {
  local target="$1" runs="$2"
  local part_size="${MULTIPART_5G_PART_SIZE:-64MiB}"
  local parts="${MULTIPART_5G_PARTS:-80}"
  local concurrency="${MULTIPART_5G_CONCURRENCY:-5}"
  local part_concurrency="${MULTIPART_5G_PART_CONCURRENCY:-20}"
  local duration="${MULTIPART_5G_DURATION:-60}"
  local bucket="bench-multipart-5g-${target}"
  local tmpdir; tmpdir="$(mktemp -d)"
  trap "rm -rf $tmpdir" RETURN

  note "workload multipart-5g: $target / part=$part_size × $parts × conc=$concurrency × part.conc=$part_concurrency × dur=${duration}s × runs=$runs (cleanup between runs)"
  local run
  for run in $(seq 1 "$runs"); do
    bucket_create "$target" "$bucket"
    warp_multipart_one "$target" "multipart-5g" "$part_size" "$parts" "$concurrency" "$part_concurrency" "$duration" "$run" "$bucket" "$tmpdir" || true
    cleanup_workload "$target" "multipart-5g"
  done
}

# Single warp list invocation (US-007). Uses warp's `list` subcommand which:
#   prepare phase: PUTs --objects objects of --obj.size shape (distributed
#     across --concurrent workers with per-worker prefixes; each worker gets
#     --objects/--concurrent objects, rounded up).
#   measure phase: each worker loops ListObjectsV2 ops against its own prefix,
#     paginated by --max-keys keys per page (full enumeration per op).
#
# warp emits op-label `LIST` (pkg/bench/list.go:211). Latency = full
# paginated-enumeration time per worker prefix; throughput = objects-listed/s
# across all workers. This is THE bucket-index claim workload — Strata's
# 64-way Cassandra/TiKV fan-out vs RGW's omap-index sequential scan. If
# Strata is slower on this row, US-012 surfaces as P1 (not P3) ROADMAP entry
# per PRD escalation rule.
#
# Note: warp's prepare phase always seeds --objects unconditionally; calling
# multiple runs against the same bucket with --noclear accumulates keys (warp
# uses fresh run-scoped prefixes so the measure phase still operates on the
# current run's slice, but the bucket grows). For PRD's 3-run × 2-target shape
# that's 3 × 100k = 300k keys per target peak ≈ 300MiB at 1KiB — negligible.
# cleanup_workload runs once at end of the workload (no between-run cleanup —
# re-seeding 100k keys per run × 3 runs would add ~3 min per target with no
# measurement benefit; the existing keys don't interfere with current-run
# measure phase which scopes to the run-specific prefix).
#
# Args: target workload size_label objects concurrency max_keys duration run_id bucket tmpdir
warp_list_one() {
  local target="$1" workload="$2" size_label="$3" objects="$4" concurrency="$5" max_keys="$6" duration="$7" run="$8" bucket="$9" tmpdir="${10}"
  local endpoint creds_ak creds_sk
  resolve_target "$target" endpoint creds_ak creds_sk
  local host
  host="$(url_host "$endpoint")"

  local summary_file="$tmpdir/warp-$target-$workload-c$concurrency-run$run.txt"
  local benchdata_file="$tmpdir/warp-$target-$workload-c$concurrency-run$run.bd"
  note "run $run: warp list $target obj=$size_label objects=$objects conc=$concurrency max-keys=$max_keys dur=${duration}s"
  set +e
  "$BENCH_TOOL_PATH" list \
    --host="$host" \
    --access-key="$creds_ak" \
    --secret-key="$creds_sk" \
    --bucket="$bucket" \
    --obj.size="$size_label" \
    --objects="$objects" \
    --concurrent="$concurrency" \
    --max-keys="$max_keys" \
    --duration="${duration}s" \
    --noclear \
    --benchdata="$benchdata_file" \
    --no-color \
    > "$summary_file" 2>&1
  local warp_rc=$?
  set -e
  if [[ "$warp_rc" -ne 0 ]]; then
    note "WARN: warp list failed on $target run $run (rc=$warp_rc) — see $summary_file"
    tail -5 "$summary_file" >&2 || true
    return 1
  fi

  # warp list prints Report: PUT. (prepare phase) + Report: LIST. (measure
  # phase). Parser anchored at start-of-line on "Report: LIST." picks the
  # measure-phase block; the prepare-phase PUT block is skipped.
  read -r p50 p95 p99 mbps ops errs < <(parse_warp_summary "$summary_file" "LIST") || true
  emit_row "$target" "$workload" "$size_label" "$concurrency" "$run" \
    "$p50" "$p95" "$p99" "$mbps" "$ops" "$errs" "$duration"
}

# ListObjects 100k-key workload (US-007). PRD: 100k keys, 1KiB each, seeded
# via parallel PUT (32 concurrent in PRD spec — warp uses single --concurrent
# for both seed + measure, so this story defaults to c=8 measure which also
# governs seed conc; --concurrency=N override flips both). Then bench phase
# measures ListObjectsV2 latency + throughput. THIS IS THE BUCKET-INDEX
# CLAIM workload — Strata's sharded fan-out (default 64 partitions per bucket)
# vs RGW's omap-index sequential scan. If Strata is SLOWER, US-012 must
# surface as **P1** ROADMAP entry (bucket-index claim regression — foundational
# README claim).
#
# Single bucket per target reused across runs; cleanup_workload at end. PRD's
# "idempotent — skip if bucket already has ≥ 100k keys" approximated by
# warp's per-run prefix isolation: each run's measure phase scopes to its
# own prefix slice, so re-running the bench doesn't re-list the same keys.
#
# Args: target runs
workload_list() {
  local target="$1" runs="$2"
  local size_label="${LIST_SIZE:-1KiB}"
  local objects="${LIST_OBJECTS:-100000}"
  local concurrency="${LIST_CONCURRENCY:-8}"
  local max_keys="${LIST_MAX_KEYS:-1000}"
  local duration="${LIST_DURATION:-60}"
  local bucket="bench-list-${target}"
  local tmpdir; tmpdir="$(mktemp -d)"
  trap "rm -rf $tmpdir" RETURN

  note "workload list: $target / $size_label × $objects objects × conc=$concurrency × max-keys=$max_keys × dur=${duration}s × runs=$runs"
  bucket_create "$target" "$bucket"
  local run
  for run in $(seq 1 "$runs"); do
    warp_list_one "$target" "list" "$size_label" "$objects" "$concurrency" "$max_keys" "$duration" "$run" "$bucket" "$tmpdir" || true
  done
  cleanup_workload "$target" "list"
}

# Single warp get invocation under explicit credentials (US-008 iam-auth).
# Mirrors warp_get_one but does NOT call resolve_target — caller passes the
# endpoint/ak/sk triple directly so the IAM-user creds (separate from the
# bench's STRATA_AK/SK admin creds) drive both warp's prepare-phase PUTs and
# its measure-phase GETs. Bucket must be writable by (endpoint, ak, sk) —
# for the iam-auth flow the bucket is created via the same IAM creds, so the
# IAM user owns the bucket (matches the Strata + RGW bucket-owner semantic).
#
# Args: target workload size_label concurrency duration run_id bucket tmpdir seed_objects endpoint ak sk
warp_get_one_creds() {
  local target="$1" workload="$2" size_label="$3" concurrency="$4" duration="$5" run="$6" bucket="$7" tmpdir="$8" seed_objects="$9"
  local endpoint="${10}" creds_ak="${11}" creds_sk="${12}"
  local host
  host="$(url_host "$endpoint")"

  local summary_file="$tmpdir/warp-$target-$workload-c$concurrency-run$run.txt"
  local benchdata_file="$tmpdir/warp-$target-$workload-c$concurrency-run$run.bd"
  note "run $run: warp get $target (iam creds) obj=$size_label conc=$concurrency dur=${duration}s seed=$seed_objects"
  set +e
  "$BENCH_TOOL_PATH" get \
    --host="$host" \
    --access-key="$creds_ak" \
    --secret-key="$creds_sk" \
    --bucket="$bucket" \
    --obj.size="$size_label" \
    --objects="$seed_objects" \
    --concurrent="$concurrency" \
    --duration="${duration}s" \
    --noclear \
    --benchdata="$benchdata_file" \
    --no-color \
    > "$summary_file" 2>&1
  local warp_rc=$?
  set -e
  if [[ "$warp_rc" -ne 0 ]]; then
    note "WARN: warp get (iam) failed on $target run $run (rc=$warp_rc) — see $summary_file"
    tail -5 "$summary_file" >&2 || true
    return 1
  fi

  read -r p50 p95 p99 mbps ops errs < <(parse_warp_summary "$summary_file" "GET") || true
  emit_row "$target" "$workload" "$size_label" "$concurrency" "$run" \
    "$p50" "$p95" "$p99" "$mbps" "$ops" "$errs" "$duration"
}

# Single warp range-get invocation (US-008 range-get). Uses warp's
# `get --range-size=<n>` mode which keeps a FIXED range size while randomising
# the offset within each seed object. PRD AC: 1000 range requests of random
# 1MiB ranges against single 100MiB object. Warp's shape:
#   prepare phase: PUT --objects of --obj.size each (default 10 × 100MiB
#     = ~1GiB seed) distributed across --concurrent workers' prefixes.
#   measure phase: each worker loops `GET Range: bytes=R-R+rs-1` (rs from
#     --range-size, random offset) against its own prefix's objects.
# warp's --range-size= flag implies --range, so we set it explicitly to land
# the fixed-1MiB-range semantic the PRD asks for; the operator-readable
# "random 1MiB" wording matches.
#
# warp emits op-label `GET` per range request (pkg/bench/get.go); parser
# anchors on `Report: GET.` which skips the prepare-phase `Report: PUT.`
# block (mirrors the get-small / get-medium / get-large / list pattern).
#
# Args: target workload size_label range_size objects concurrency duration run_id bucket tmpdir
warp_range_get_one() {
  local target="$1" workload="$2" size_label="$3" range_size="$4" objects="$5" concurrency="$6" duration="$7" run="$8" bucket="$9" tmpdir="${10}"
  local endpoint creds_ak creds_sk
  resolve_target "$target" endpoint creds_ak creds_sk
  local host
  host="$(url_host "$endpoint")"

  local summary_file="$tmpdir/warp-$target-$workload-c$concurrency-run$run.txt"
  local benchdata_file="$tmpdir/warp-$target-$workload-c$concurrency-run$run.bd"
  note "run $run: warp range-get $target obj=$size_label range=$range_size objects=$objects conc=$concurrency dur=${duration}s"
  set +e
  "$BENCH_TOOL_PATH" get \
    --host="$host" \
    --access-key="$creds_ak" \
    --secret-key="$creds_sk" \
    --bucket="$bucket" \
    --obj.size="$size_label" \
    --objects="$objects" \
    --range-size="$range_size" \
    --concurrent="$concurrency" \
    --duration="${duration}s" \
    --noclear \
    --benchdata="$benchdata_file" \
    --no-color \
    > "$summary_file" 2>&1
  local warp_rc=$?
  set -e
  if [[ "$warp_rc" -ne 0 ]]; then
    note "WARN: warp range-get failed on $target run $run (rc=$warp_rc) — see $summary_file"
    tail -5 "$summary_file" >&2 || true
    return 1
  fi

  read -r p50 p95 p99 mbps ops errs < <(parse_warp_summary "$summary_file" "GET") || true
  # object_size column in JSONL carries the *range* size — that's the IO unit
  # of the measure phase (the seed object size is captured in the workload
  # label `range-get`).
  emit_row "$target" "$workload" "$range_size" "$concurrency" "$run" \
    "$p50" "$p95" "$p99" "$mbps" "$ops" "$errs" "$duration"
}

# Single warp delete invocation (US-008 delete). Warp's `delete` subcommand
#   prepare phase: PUTs --objects --obj.size objects across --concurrent
#     worker prefixes (each worker gets --objects/--concurrent rounded up).
#   measure phase: each worker DELETEs from its own prefix in batches of
#     --batch (default 100). Measure ends when either all objects are
#     deleted OR --duration elapses, whichever comes first.
#
# Op-label `DELETE` per pkg/bench/delete.go; throughput is objects-deleted/s.
# Throughput in MiB/s is reported by warp but reflects bytes-deleted/s which
# is ≈ 0 for the 1KiB seed shape — focus on ops/s + p99.
#
# PRD spec: 1000 small objects deleted in concurrency 8 after seed phase.
# Maps cleanly: --objects=1000 --concurrent=8 --duration=60s.
#
# warp enforces `objects >= concurrency * batch * 4` (pkg/bench/delete.go). With
# the default --batch=100 + concurrency=8 the floor is 3200, which would
# violate the PRD's 1000-object spec. We auto-pick batch=max(1, objects/
# (concurrency*4)) so 1000 + c=8 lands batch=31, comfortably under the floor.
# Operator can pin via DELETE_BATCH env if a specific shape is required.
#
# Args: target workload size_label objects concurrency duration run_id bucket tmpdir
warp_delete_one() {
  local target="$1" workload="$2" size_label="$3" objects="$4" concurrency="$5" duration="$6" run="$7" bucket="$8" tmpdir="$9"
  local endpoint creds_ak creds_sk
  resolve_target "$target" endpoint creds_ak creds_sk
  local host
  host="$(url_host "$endpoint")"

  local batch="${DELETE_BATCH:-}"
  if [[ -z "$batch" ]]; then
    batch="$(awk -v o="$objects" -v c="$concurrency" 'BEGIN { b=int(o/(c*4)); if (b<1) b=1; printf("%d", b) }')"
  fi

  local summary_file="$tmpdir/warp-$target-$workload-c$concurrency-run$run.txt"
  local benchdata_file="$tmpdir/warp-$target-$workload-c$concurrency-run$run.bd"
  note "run $run: warp delete $target obj=$size_label objects=$objects conc=$concurrency batch=$batch dur=${duration}s"
  set +e
  "$BENCH_TOOL_PATH" delete \
    --host="$host" \
    --access-key="$creds_ak" \
    --secret-key="$creds_sk" \
    --bucket="$bucket" \
    --obj.size="$size_label" \
    --objects="$objects" \
    --concurrent="$concurrency" \
    --batch="$batch" \
    --duration="${duration}s" \
    --noclear \
    --benchdata="$benchdata_file" \
    --no-color \
    > "$summary_file" 2>&1
  local warp_rc=$?
  set -e
  if [[ "$warp_rc" -ne 0 ]]; then
    note "WARN: warp delete failed on $target run $run (rc=$warp_rc) — see $summary_file"
    tail -5 "$summary_file" >&2 || true
    return 1
  fi

  # warp delete prints Report: PUT. (prepare) + Report: DELETE. (measure).
  # Parser anchored at start-of-line on "Report: DELETE." picks the measure
  # block; prepare-phase PUT block is skipped.
  read -r p50 p95 p99 mbps ops errs < <(parse_warp_summary "$summary_file" "DELETE") || true
  emit_row "$target" "$workload" "$size_label" "$concurrency" "$run" \
    "$p50" "$p95" "$p99" "$mbps" "$ops" "$errs" "$duration"
}

# -----------------------------------------------------------------------------
# IAM helpers (US-008 iam-auth workload)
#
# Sets IAM_AK / IAM_SK globals on success; returns 1 (and logs WARN) on
# failure so the iam-auth workload can decide whether to skip the run.
# Idempotent on the "user already exists" path — RGW radosgw-admin returns
# the existing user info on a duplicate `user create`; Strata's admin API
# returns 409 EntityAlreadyExists which the helper treats as "look up keys
# instead". Cleanup helpers are best-effort (|| true) so a half-created
# user does not strand the bench.
# -----------------------------------------------------------------------------

IAM_AK=""
IAM_SK=""

# Login to Strata admin API with the static bench admin creds, drop the
# session cookie into the supplied jar file. Returns 1 on failure.
# US-009 generalised to accept an explicit endpoint so the cassandra target
# reuses the exact same flow against the strata-cassandra gateway on port 9998.
strata_admin_login_at() {
  local endpoint="$1" jar="$2"
  local code
  code="$(curl -sS -o "$jar.body" -w '%{http_code}' \
    -c "$jar" \
    -X POST "$endpoint/admin/v1/auth/login" \
    -H 'Content-Type: application/json' \
    -d "{\"access_key\":\"$STRATA_AK\",\"secret_key\":\"$STRATA_SK\"}" \
    2>/dev/null || echo 000)"
  if [[ "$code" != "200" ]]; then
    note "WARN: $endpoint/admin/v1/auth/login returned HTTP $code"
    [[ -f "$jar.body" ]] && tail -3 "$jar.body" >&2 || true
    return 1
  fi
  return 0
}

strata_admin_login() { strata_admin_login_at "$STRATA_ENDPOINT_URL" "$1"; }

# Strata: create IAM user + mint an access key. Idempotent on EntityAlreadyExists
# (409) — falls through to look up an existing key (or mints a fresh one when
# none exist). Sets IAM_AK / IAM_SK on success. The endpoint argument lets the
# US-009 cassandra target reuse the function pointed at the Strata-Cassandra
# gateway on port 9998 (admin API surface is identical between meta backends).
create_iam_user_strata_at() {
  local endpoint="$1" user="$2"
  local jar; jar="$(mktemp)"
  if ! strata_admin_login_at "$endpoint" "$jar"; then
    rm -f "$jar" "$jar.body"
    return 1
  fi

  # Create user (tolerate 409 EntityAlreadyExists).
  local code
  code="$(curl -sS -o "$jar.body" -w '%{http_code}' \
    -b "$jar" \
    -X POST "$endpoint/admin/v1/iam/users" \
    -H 'Content-Type: application/json' \
    -d "{\"user_name\":\"$user\"}" 2>/dev/null || echo 000)"
  if [[ "$code" != "200" && "$code" != "201" && "$code" != "204" && "$code" != "409" ]]; then
    note "WARN: $endpoint POST /admin/v1/iam/users returned HTTP $code"
    [[ -f "$jar.body" ]] && tail -3 "$jar.body" >&2 || true
    rm -f "$jar" "$jar.body"
    return 1
  fi

  # Mint a fresh access key — only response that carries the secret.
  local resp
  resp="$(curl -sS -b "$jar" \
    -X POST "$endpoint/admin/v1/iam/users/$user/access-keys" 2>/dev/null)"
  IAM_AK="$(printf '%s' "$resp" | jq -r '.access_key_id // empty' 2>/dev/null)"
  IAM_SK="$(printf '%s' "$resp" | jq -r '.secret_access_key // empty' 2>/dev/null)"
  rm -f "$jar" "$jar.body"
  if [[ -z "$IAM_AK" || -z "$IAM_SK" ]]; then
    note "WARN: $endpoint POST /admin/v1/iam/users/$user/access-keys did not return access_key_id+secret_access_key"
    note "  response (first 200 chars): $(printf '%s' "$resp" | head -c 200)"
    return 1
  fi
  note "  iam_user ($endpoint): $user / ak=${IAM_AK:0:6}…"
  return 0
}

create_iam_user_strata() { create_iam_user_strata_at "$STRATA_ENDPOINT_URL" "$1"; }

# Strata: cleanup IAM user — drops every access key then the user.
cleanup_iam_user_strata_at() {
  local endpoint="$1" user="$2"
  local jar; jar="$(mktemp)"
  if ! strata_admin_login_at "$endpoint" "$jar"; then
    rm -f "$jar" "$jar.body"
    return 0
  fi
  # List keys, delete each. Errors swallowed — cleanup is best-effort.
  local keys
  keys="$(curl -sS -b "$jar" "$endpoint/admin/v1/iam/users/$user/access-keys" 2>/dev/null \
    | jq -r '.access_keys[]?.access_key_id // empty' 2>/dev/null)"
  local k
  for k in $keys; do
    curl -sS -b "$jar" -X DELETE "$endpoint/admin/v1/iam/access-keys/$k" >/dev/null 2>&1 || true
  done
  curl -sS -b "$jar" -X DELETE "$endpoint/admin/v1/iam/users/$user" >/dev/null 2>&1 || true
  rm -f "$jar" "$jar.body"
}

cleanup_iam_user_strata() { cleanup_iam_user_strata_at "$STRATA_ENDPOINT_URL" "$1"; }

# RGW: create bench user via `radosgw-admin user create --uid=...`. RGW's user
# create returns full user JSON including keys[0].{access_key,secret_key}. On
# duplicate user (already exists), `user info --uid=...` returns the same
# shape. Sets IAM_AK / IAM_SK on success.
create_iam_user_rgw() {
  local user="$1"
  local compose
  compose="$(compose_cmd)"
  local out
  out="$($compose exec -T rgw radosgw-admin user create --uid="$user" --display-name="Bench IAM" 2>/dev/null || true)"
  if [[ -z "$out" || "$(printf '%s' "$out" | jq -r '.keys[0].access_key // empty' 2>/dev/null)" == "" ]]; then
    # Fall back to user info (user already exists path).
    out="$($compose exec -T rgw radosgw-admin user info --uid="$user" 2>/dev/null || true)"
  fi
  IAM_AK="$(printf '%s' "$out" | jq -r '.keys[0].access_key // empty' 2>/dev/null)"
  IAM_SK="$(printf '%s' "$out" | jq -r '.keys[0].secret_key // empty' 2>/dev/null)"
  if [[ -z "$IAM_AK" || -z "$IAM_SK" ]]; then
    note "WARN: rgw radosgw-admin user create/info for '$user' returned no keys"
    return 1
  fi
  note "  iam_user (rgw): $user / ak=${IAM_AK:0:6}…"
  return 0
}

cleanup_iam_user_rgw() {
  local user="$1"
  local compose
  compose="$(compose_cmd)"
  $compose exec -T rgw radosgw-admin user rm --uid="$user" --purge-data >/dev/null 2>&1 || true
}

create_iam_user() {
  local target="$1" user="$2"
  case "$target" in
    strata)    create_iam_user_strata "$user" ;;
    rgw)       create_iam_user_rgw "$user" ;;
    # US-009: same admin-API surface against the Strata-Cassandra gateway.
    cassandra) create_iam_user_strata_at "$CASSANDRA_ENDPOINT_URL" "$user" ;;
    *) note "WARN: create_iam_user unknown target $target"; return 1 ;;
  esac
}

cleanup_iam_user() {
  local target="$1" user="$2"
  case "$target" in
    strata)    cleanup_iam_user_strata "$user" ;;
    rgw)       cleanup_iam_user_rgw "$user" ;;
    cassandra) cleanup_iam_user_strata_at "$CASSANDRA_ENDPOINT_URL" "$user" ;;
    *) return 0 ;;
  esac
}

# Cleanup bucket using arbitrary credentials (vs the admin-only cleanup_workload
# above). Used by iam-auth where the bucket is owned by the IAM user and the
# admin AK doesn't have implicit access to it on every backend.
#
# Args: endpoint ak sk bucket
cleanup_bucket_with_creds() {
  local endpoint="$1" creds_ak="$2" creds_sk="$3" bucket="$4"
  AWS_ACCESS_KEY_ID="$creds_ak" AWS_SECRET_ACCESS_KEY="$creds_sk" AWS_DEFAULT_REGION=us-east-1 \
    aws --endpoint-url "$endpoint" s3 rm "s3://$bucket" --recursive >/dev/null 2>&1 || true
  AWS_ACCESS_KEY_ID="$creds_ak" AWS_SECRET_ACCESS_KEY="$creds_sk" AWS_DEFAULT_REGION=us-east-1 \
    aws --endpoint-url "$endpoint" s3api delete-bucket --bucket "$bucket" >/dev/null 2>&1 || true
}

# -----------------------------------------------------------------------------
# US-008 workloads
# -----------------------------------------------------------------------------

# Range-GET workload (US-008). Random 1MiB ranges via warp `get --range-size=`.
# Single bucket per target reused across runs; cleanup_workload at end. Mirrors
# the list workload's run-shape — between-run re-seed would burn IO without
# changing the measurement (each run's measure phase covers its own range
# distribution against the seed).
#
# PRD: 1000 range requests of random 1MiB ranges against single 100MB object,
# concurrency 8. Mapping deviation documented in the helper above: warp's
# range-size mode requires --objects ≥ 1 per worker, so we seed 10 × 100MiB
# objects (~1GiB) and distribute requests over them; the per-request shape
# (Range: bytes=R-R+1MiB-1) is identical to the PRD's literal spec. Operator
# can drop RANGE_GET_OBJECTS to 1 + RANGE_GET_CONCURRENCY=1 for the literal
# single-object shape, at the cost of zero concurrency.
#
# Args: target runs
workload_range_get() {
  local target="$1" runs="$2"
  local size_label="${RANGE_GET_SIZE:-100MiB}"
  local objects="${RANGE_GET_OBJECTS:-10}"
  local range_size="${RANGE_GET_RANGE_SIZE:-1MiB}"
  local concurrency="${RANGE_GET_CONCURRENCY:-8}"
  local duration="${RANGE_GET_DURATION:-60}"
  local bucket="bench-range-get-${target}"
  local tmpdir; tmpdir="$(mktemp -d)"
  trap "rm -rf $tmpdir" RETURN

  note "workload range-get: $target / $size_label × $objects objects × range=$range_size × conc=$concurrency × dur=${duration}s × runs=$runs"
  bucket_create "$target" "$bucket"
  local run
  for run in $(seq 1 "$runs"); do
    warp_range_get_one "$target" "range-get" "$size_label" "$range_size" "$objects" "$concurrency" "$duration" "$run" "$bucket" "$tmpdir" || true
  done
  cleanup_workload "$target" "range-get"
}

# Delete workload (US-008). warp `delete` — seeds N small objects then DELETEs
# them. Cleanup-between-runs because warp's measure phase ends when the seed
# is exhausted OR --duration elapses; recreating the bucket per run gives a
# fresh seed each time and keeps the per-run shape consistent.
#
# Args: target runs
workload_delete() {
  local target="$1" runs="$2"
  local size_label="${DELETE_SIZE:-1KiB}"
  local objects="${DELETE_OBJECTS:-1000}"
  local concurrency="${DELETE_CONCURRENCY:-8}"
  local duration="${DELETE_DURATION:-60}"
  local bucket="bench-delete-${target}"
  local tmpdir; tmpdir="$(mktemp -d)"
  trap "rm -rf $tmpdir" RETURN

  note "workload delete: $target / $size_label × $objects objects × conc=$concurrency × dur=${duration}s × runs=$runs (cleanup between runs)"
  local run
  for run in $(seq 1 "$runs"); do
    bucket_create "$target" "$bucket"
    warp_delete_one "$target" "delete" "$size_label" "$objects" "$concurrency" "$duration" "$run" "$bucket" "$tmpdir" || true
    cleanup_workload "$target" "delete"
  done
}

# IAM-auth workload (US-008). Pre-create IAM user via target admin API, run
# warp `get` under the IAM user's SigV4 credentials. The IAM user creates and
# owns the bench bucket (matches Strata + RGW bucket-owner semantic), so no
# explicit policy attach is required — owner has implicit full access on both
# backends. Cleanup drops the bucket via IAM creds (admin AK doesn't own it),
# then deletes the IAM user via admin API.
#
# Why this workload differs from get-small (US-004): get-small drives warp
# with the static admin/owner creds, which the gateway resolves to the
# in-process static credential store. iam-auth resolves the SigV4 access key
# through the meta-backed IAM credentials path (cassandra/tikv access_keys
# table → IAMCredentialStore.Lookup → MultiStore cache), exercising the
# identity-resolve hot path that production IAM workflows hit.
#
# Args: target runs
workload_iam_auth() {
  local target="$1" runs="$2"
  local size_label="${IAM_AUTH_SIZE:-1KiB}"
  local objects="${IAM_AUTH_OBJECTS:-1000}"
  local concurrency="${IAM_AUTH_CONCURRENCY:-8}"
  local duration="${IAM_AUTH_DURATION:-60}"
  local user_base="${IAM_AUTH_USER:-bench-iam}"
  # Per-invocation suffix on both user + bucket names: an interrupted prior
  # run leaves an IAM-owned bucket that the admin AK cannot delete (Strata's
  # bucket-owner ACL gates DeleteBucket). With a fresh suffix per invocation,
  # orphans don't block the next bench run. The cleanup_bucket_with_creds at
  # the end of the workload does the best-effort drop while the IAM key is
  # still valid.
  local stamp; stamp="$(date +%s)"
  local user="${user_base}-${target}-${stamp}"
  local bucket="bench-iam-auth-${target}-${stamp}"
  local tmpdir; tmpdir="$(mktemp -d)"
  trap "rm -rf $tmpdir" RETURN

  note "workload iam-auth: $target / $size_label × $objects objects × conc=$concurrency × dur=${duration}s × runs=$runs"
  IAM_AK=""; IAM_SK=""
  if ! create_iam_user "$target" "$user"; then
    note "WARN: iam-auth: failed to create IAM user '$user' on $target — skipping"
    return 0
  fi

  local endpoint
  case "$target" in
    strata)    endpoint="$STRATA_ENDPOINT_URL" ;;
    rgw)       endpoint="$RGW_ENDPOINT_URL" ;;
    cassandra) endpoint="$CASSANDRA_ENDPOINT_URL" ;;
    *) note "WARN: iam-auth unknown target $target"; cleanup_iam_user "$target" "$user"; return 0 ;;
  esac

  # Bucket owned by the IAM user — warp's prepare phase creates the bucket
  # via authenticated PUT under the IAM creds if it doesn't already exist.
  # We don't pre-create via admin AK because that would make the bucket
  # admin-owned and either Strata's owner-mismatch ACL or RGW's similar
  # check could fail subsequent IAM-user PUTs.
  AWS_ACCESS_KEY_ID="$IAM_AK" AWS_SECRET_ACCESS_KEY="$IAM_SK" AWS_DEFAULT_REGION=us-east-1 \
    aws --endpoint-url "$endpoint" s3api create-bucket --bucket "$bucket" >/dev/null 2>&1 || true

  local run
  for run in $(seq 1 "$runs"); do
    warp_get_one_creds "$target" "iam-auth" "$size_label" "$concurrency" "$duration" "$run" "$bucket" "$tmpdir" "$objects" "$endpoint" "$IAM_AK" "$IAM_SK" || true
  done

  cleanup_bucket_with_creds "$endpoint" "$IAM_AK" "$IAM_SK" "$bucket"
  cleanup_iam_user "$target" "$user"
}

# -----------------------------------------------------------------------------
# Workload dispatch
# -----------------------------------------------------------------------------

# Resolve sweep array from $CONCURRENCY_SWEEP (comma-separated). Falls back
# to the 1/8/32/128 US-004 default if unset / empty.
resolve_sweep() {
  local raw="${1:-${CONCURRENCY_SWEEP:-1,8,32,128}}"
  local -n _arr="$2"
  IFS=',' read -ra _arr <<< "$raw"
  if [[ "${#_arr[@]}" -eq 0 ]]; then
    abort "concurrency sweep parsed to empty list from '$raw'"
  fi
  local c
  for c in "${_arr[@]}"; do
    if [[ ! "$c" =~ ^[0-9]+$ ]] || (( c < 1 )); then
      abort "concurrency sweep value '$c' not a positive integer"
    fi
  done
}

dispatch_workload() {
  local workload="$1" target="$2" concurrency="$3" runs="$4"
  local r="${runs:-3}"
  case "$workload" in
    put-small)
      # 1KiB PUT. US-004 default = concurrency sweep {1,8,32,128} × 60s × 3 runs.
      # Single-point shape (US-002 backwards compat) when --concurrency=N set.
      local size_label="${PUT_SMALL_SIZE:-1KiB}"
      local duration="${PUT_SMALL_DURATION:-60}"
      if [[ -n "$concurrency" ]]; then
        workload_put "$target" "put-small" "$size_label" "$concurrency" "$duration" "$r"
      else
        local sweep_arr=()
        resolve_sweep "" sweep_arr
        workload_put_sweep "$target" "put-small" "$size_label" "$duration" "$r" "${sweep_arr[@]}"
      fi
      ;;
    put-medium)
      # 1MiB PUT. US-004: concurrency sweep {1,8,32,128} × 60s × 3 runs.
      local size_label="${PUT_MEDIUM_SIZE:-1MiB}"
      local duration="${PUT_MEDIUM_DURATION:-60}"
      if [[ -n "$concurrency" ]]; then
        workload_put "$target" "put-medium" "$size_label" "$concurrency" "$duration" "$r"
      else
        local sweep_arr=()
        resolve_sweep "" sweep_arr
        workload_put_sweep "$target" "put-medium" "$size_label" "$duration" "$r" "${sweep_arr[@]}"
      fi
      ;;
    get-small)
      # 1KiB GET (warp auto-seeds bucket to --objects=N).
      # US-004: concurrency sweep {1,8,32,128} × 60s × 3 runs.
      # Seed sized to absorb high-concurrency draws without trivial cache hits
      # (10000 objects = 10MB on-disk for 1KiB shape).
      local size_label="${GET_SMALL_SIZE:-1KiB}"
      local duration="${GET_SMALL_DURATION:-60}"
      local seed="${GET_SMALL_OBJECTS:-10000}"
      if [[ -n "$concurrency" ]]; then
        local bucket="bench-get-small-${target}"
        local tmpdir; tmpdir="$(mktemp -d)"
        trap "rm -rf $tmpdir" RETURN
        bucket_create "$target" "$bucket"
        warp_get_runs "$target" "get-small" "$size_label" "$concurrency" "$duration" "$r" "$bucket" "$tmpdir" "$seed"
        cleanup_workload "$target" "get-small"
      else
        local sweep_arr=()
        resolve_sweep "" sweep_arr
        workload_get_sweep "$target" "get-small" "$size_label" "$duration" "$r" "$seed" "${sweep_arr[@]}"
      fi
      ;;
    get-medium)
      # 1MiB GET. US-004: concurrency sweep {1,8,32,128} × 60s × 3 runs.
      # Seed default 2500 objects = 2.5GB on-disk.
      local size_label="${GET_MEDIUM_SIZE:-1MiB}"
      local duration="${GET_MEDIUM_DURATION:-60}"
      local seed="${GET_MEDIUM_OBJECTS:-2500}"
      if [[ -n "$concurrency" ]]; then
        local bucket="bench-get-medium-${target}"
        local tmpdir; tmpdir="$(mktemp -d)"
        trap "rm -rf $tmpdir" RETURN
        bucket_create "$target" "$bucket"
        warp_get_runs "$target" "get-medium" "$size_label" "$concurrency" "$duration" "$r" "$bucket" "$tmpdir" "$seed"
        cleanup_workload "$target" "get-medium"
      else
        local sweep_arr=()
        resolve_sweep "" sweep_arr
        workload_get_sweep "$target" "get-medium" "$size_label" "$duration" "$r" "$seed" "${sweep_arr[@]}"
      fi
      ;;
    put-large)
      # 100MiB PUT × c=4 × 60s × 3 runs (US-005). Single conc point per PRD
      # (large-object shape exercises chunking / manifest / network buffer, not
      # the small-object scheduling shape US-004 sweeps). cleanup_workload
      # runs BETWEEN runs to stay under the 300GB disk pre-flight budget.
      # --concurrency=N override flips the default c=4.
      if [[ -n "$concurrency" ]]; then
        PUT_LARGE_CONCURRENCY="$concurrency" workload_put_large "$target" "$r"
      else
        workload_put_large "$target" "$r"
      fi
      ;;
    get-large)
      # 100MiB GET × c=4 × 60s × 3 runs (US-005). Mirrors put-large shape:
      # single conc point, cleanup between runs. warp auto-seeds bucket to
      # --objects=50 (= ~5GiB seed) per run.
      if [[ -n "$concurrency" ]]; then
        GET_LARGE_CONCURRENCY="$concurrency" workload_get_large "$target" "$r"
      else
        workload_get_large "$target" "$r"
      fi
      ;;
    multipart-5g)
      # warp multipart-put: 5 parallel multipart uploads of 5GiB each
      # (80 × 64MiB parts) × 60s × 3 runs (US-006). PRD AC says 5 GB per
      # upload — see workload_multipart_5g for the mapping. cleanup between
      # runs mandatory to stay under the 300GB disk pre-flight budget.
      # --concurrency=N overrides the default c=5.
      if [[ -n "$concurrency" ]]; then
        MULTIPART_5G_CONCURRENCY="$concurrency" workload_multipart_5g "$target" "$r"
      else
        workload_multipart_5g "$target" "$r"
      fi
      ;;
    list)
      # ListObjects 100k-key workload (US-007) — THE bucket-index claim
      # workload. warp `list` op-label LIST (--max-keys=1000 paginated full
      # enumeration per worker prefix). Single bucket per target reused
      # across runs; cleanup_workload at end. --concurrency=N overrides
      # the default c=8 (governs both seed-phase PUT conc + measure-phase
      # LIST conc — warp shares one --concurrent flag).
      if [[ -n "$concurrency" ]]; then
        LIST_CONCURRENCY="$concurrency" workload_list "$target" "$r"
      else
        workload_list "$target" "$r"
      fi
      ;;
    range-get)
      # Random fixed-1MiB ranges via warp `get --range-size=1MiB` (US-008).
      # Single bucket per target reused across runs; cleanup_workload at end.
      # --concurrency=N overrides the default c=8.
      if [[ -n "$concurrency" ]]; then
        RANGE_GET_CONCURRENCY="$concurrency" workload_range_get "$target" "$r"
      else
        workload_range_get "$target" "$r"
      fi
      ;;
    delete)
      # warp `delete` — seeds N small objects then DELETEs them (US-008).
      # Cleanup between runs (warp's measure phase ends when seed exhausted;
      # recreating bucket per run gives consistent per-run shape).
      # --concurrency=N overrides the default c=8.
      if [[ -n "$concurrency" ]]; then
        DELETE_CONCURRENCY="$concurrency" workload_delete "$target" "$r"
      else
        workload_delete "$target" "$r"
      fi
      ;;
    iam-auth)
      # Pre-create IAM user via target admin API, run warp `get` under the
      # IAM user's SigV4 creds (US-008). The IAM user creates and owns the
      # bench bucket; cleanup drops bucket via IAM creds then deletes user
      # via admin API. --concurrency=N overrides the default c=8.
      if [[ -n "$concurrency" ]]; then
        IAM_AUTH_CONCURRENCY="$concurrency" workload_iam_auth "$target" "$r"
      else
        workload_iam_auth "$target" "$r"
      fi
      ;;
    *)
      abort "workload '$workload' not implemented (US-002 ships put-small; US-004 adds put-medium/get-small/get-medium; US-005 adds put-large/get-large; US-006 adds multipart-5g; US-007 adds list; US-008 adds range-get/delete/iam-auth)"
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
  echo "## Flat summary — per (target, workload, concurrency) point"
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

  echo ""
  echo "## Per-workload concurrency-sweep pivot (US-004)"
  echo ""
  echo "One block per workload. Rows = concurrency point; cols = Strata-TiKV p99 / RGW p99 / ratio (Strata-TiKV/RGW) / combined p99 stddev (sqrt(strata² + rgw²), error propagation). ratio > 1 = Strata-TiKV slower; ratio < 1 = Strata-TiKV faster. When --include-cassandra was used (US-009), an additional **Strata-Cassandra p99** column is emitted plus a **ratio (Strata-Cassandra/Strata-TiKV)** column for backend perf delta."
  echo ""

  # US-009: detect presence of cassandra rows in the JSONL — if any exist,
  # the pivot grid widens to a 7-column 3-way comparison; otherwise the
  # 5-column 2-target shape is preserved (backwards-compat with US-004 doc).
  local has_cassandra
  has_cassandra="$(jq -r 'select(.target=="cassandra") | .target' "$RESULTS_FILE" | head -1)"

  # Build the unique workload list and emit one pivot per workload that has
  # both targets present (single-target workloads get a flat strata-only or
  # rgw-only table, with ratio/combined-sd columns left blank).
  local workloads
  workloads="$(jq -r '.workload' "$RESULTS_FILE" | sort -u)"
  local wl
  for wl in $workloads; do
    [[ -z "$wl" ]] && continue
    # Resolve object_size for the workload (use first row's size — workload
    # is intentionally single-size per US-004 design).
    local sz
    sz="$(jq -r --arg wl "$wl" 'select(.workload==$wl) | .object_size' "$RESULTS_FILE" | head -1)"
    echo "### ${wl} (object_size: ${sz})"
    echo ""
    if [[ -n "$has_cassandra" ]]; then
      printf '| concurrency | Strata-TiKV p99 (ms) | RGW p99 (ms) | Strata-Cassandra p99 (ms) | ratio (TiKV/RGW) | ratio (Cass/TiKV) | combined p99 stddev |\n'
      printf '|-------------|----------------------|--------------|---------------------------|------------------|-------------------|---------------------|\n'
    else
      printf '| concurrency | Strata p99 (ms, mean±sd) | RGW p99 (ms, mean±sd) | ratio (Strata/RGW) | combined p99 stddev |\n'
      printf '|-------------|--------------------------|------------------------|--------------------|---------------------|\n'
    fi

    local concs
    concs="$(jq -r --arg wl "$wl" 'select(.workload==$wl) | .concurrency' "$RESULTS_FILE" | sort -un)"
    local c
    for c in $concs; do
      [[ -z "$c" ]] && continue
      if [[ -n "$has_cassandra" ]]; then
        # US-009 3-way row. jq emits "<conc>|<tikv>|<rgw>|<cass>|<r_tr>|<r_ct>|<combined>".
        local row
        row="$(jq -rs --arg wl "$wl" --argjson c "$c" '
          def stats($a):
            ($a | add / length) as $m
            | { mean: $m,
                sd: ($a | map(. - $m) | map(. * .) | add / length | sqrt) };
          map(select(.workload==$wl and .concurrency==$c))
          | (map(select(.target=="strata") | .p99_ms)) as $sp
          | (map(select(.target=="rgw") | .p99_ms)) as $rp
          | (map(select(.target=="cassandra") | .p99_ms)) as $cp
          | (if ($sp|length) > 0 then stats($sp) else null end) as $sstats
          | (if ($rp|length) > 0 then stats($rp) else null end) as $rstats
          | (if ($cp|length) > 0 then stats($cp) else null end) as $cstats
          | { sm: (if $sstats then $sstats.mean else null end),
              ss: (if $sstats then $sstats.sd else null end),
              rm: (if $rstats then $rstats.mean else null end),
              rs: (if $rstats then $rstats.sd else null end),
              cm: (if $cstats then $cstats.mean else null end),
              cs: (if $cstats then $cstats.sd else null end) }
          | (if .sm and .rm and .rm > 0 then (.sm/.rm) else null end) as $r_tr
          | (if .cm and .sm and .sm > 0 then (.cm/.sm) else null end) as $r_ct
          | (if .ss and .rs then (.ss*.ss + .rs*.rs | sqrt) else null end) as $combined
          | "\($c)|\(if .sm then "\(.sm|tostring|.[:6])±\(.ss|tostring|.[:5])" else "—" end)|\(if .rm then "\(.rm|tostring|.[:6])±\(.rs|tostring|.[:5])" else "—" end)|\(if .cm then "\(.cm|tostring|.[:6])±\(.cs|tostring|.[:5])" else "—" end)|\(if $r_tr then ($r_tr|tostring|.[:5]) else "—" end)|\(if $r_ct then ($r_ct|tostring|.[:5]) else "—" end)|\(if $combined then ($combined|tostring|.[:5]) else "—" end)"
        ' "$RESULTS_FILE")"
        IFS='|' read -r pcol scol rcol ccol r_tr r_ct combinedcol <<< "$row"
        printf '| %s | %s | %s | %s | %s | %s | %s |\n' "$pcol" "$scol" "$rcol" "$ccol" "$r_tr" "$r_ct" "$combinedcol"
      else
        # 2-target shape (US-004 backwards-compat).
        local row
        row="$(jq -rs --arg wl "$wl" --argjson c "$c" '
          def stats($a):
            ($a | add / length) as $m
            | { mean: $m,
                sd: ($a | map(. - $m) | map(. * .) | add / length | sqrt) };
          map(select(.workload==$wl and .concurrency==$c))
          | (map(select(.target=="strata") | .p99_ms)) as $sp
          | (map(select(.target=="rgw") | .p99_ms)) as $rp
          | (if ($sp|length) > 0 then stats($sp) else null end) as $sstats
          | (if ($rp|length) > 0 then stats($rp) else null end) as $rstats
          | { sm: (if $sstats then $sstats.mean else null end),
              ss: (if $sstats then $sstats.sd else null end),
              rm: (if $rstats then $rstats.mean else null end),
              rs: (if $rstats then $rstats.sd else null end) }
          | (if .sm and .rm and .rm > 0 then (.sm/.rm) else null end) as $ratio
          | (if .ss and .rs then (.ss*.ss + .rs*.rs | sqrt) else null end) as $combined
          | "\($c)|\(if .sm then "\(.sm|tostring|.[:6])±\(.ss|tostring|.[:5])" else "—" end)|\(if .rm then "\(.rm|tostring|.[:6])±\(.rs|tostring|.[:5])" else "—" end)|\(if $ratio then ($ratio|tostring|.[:5]) else "—" end)|\(if $combined then ($combined|tostring|.[:5]) else "—" end)"
        ' "$RESULTS_FILE")"
        IFS='|' read -r pcol scol rcol ratiocol combinedcol <<< "$row"
        printf '| %s | %s | %s | %s | %s |\n' "$pcol" "$scol" "$rcol" "$ratiocol" "$combinedcol"
      fi
    done
    echo ""
    if [[ -n "$has_cassandra" ]]; then
      echo "_Conclusion_: <operator fills in after run — 3-way framing per US-009: e.g. \"TiKV vs Cassandra on the same RADOS cluster shows X% delta; the choice is workload-dependent.\"_"
    else
      echo "_Conclusion_: <operator fills in after run — e.g. \"Strata 1KB PUT scales sub-linearly at c=128 vs RGW; both saturate around 4000 ops/sec on lima lab\"._"
    fi
    echo ""
  done
}

# -----------------------------------------------------------------------------
# Main
# -----------------------------------------------------------------------------

CONCURRENCY=""
CONCURRENCY_SWEEP="${CONCURRENCY_SWEEP:-}"
RUNS=""
MODE="run"
POSITIONAL=()

while (($#)); do
  case "$1" in
    --concurrency=*) CONCURRENCY="${1#*=}" ;;
    --concurrency-sweep=*) CONCURRENCY_SWEEP="${1#*=}" ;;
    --runs=*) RUNS="${1#*=}" ;;
    --report) MODE="report" ;;
    --extract-rgw-creds) MODE="extract-rgw-creds" ;;
    --preflight) MODE="preflight" ;;
    --skip-preflight) SKIP_PREFLIGHT=1 ;;
    --include-cassandra) INCLUDE_CASSANDRA=1 ;;
    -h|--help) sed -n '1,70p' "$0"; exit 0 ;;
    --) shift; break ;;
    -*) abort "unknown flag: $1" ;;
    *) POSITIONAL+=("$1") ;;
  esac
  shift
done

if [[ -n "$CONCURRENCY" && -n "$CONCURRENCY_SWEEP" ]]; then
  abort "--concurrency=N and --concurrency-sweep=A,B,C are mutually exclusive"
fi

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
    if [[ "$TARGET" == "rgw" || "$TARGET" == "both" || "$TARGET" == "all" ]]; then
      extract_rgw_creds
    fi
    # US-009: cassandra/all targets need the cassandra lab reachable.
    if (( INCLUDE_CASSANDRA )) || [[ "$TARGET" == "cassandra" || "$TARGET" == "all" ]]; then
      ensure_cassandra_lab
    fi
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
if [[ "$TARGET" == "rgw" || "$TARGET" == "both" || "$TARGET" == "all" ]]; then
  extract_rgw_creds
fi

# US-009: cassandra/all targets need strata-cassandra running. --include-cassandra
# auto-boots; without the flag we require an already-up lab and abort otherwise.
if [[ "$TARGET" == "cassandra" || "$TARGET" == "all" ]]; then
  if (( INCLUDE_CASSANDRA )); then
    ensure_cassandra_lab
  else
    code="$(probe_target "$CASSANDRA_ENDPOINT_URL")"
    if [[ "$code" != "200" && "$code" != "403" ]]; then
      abort "target '$TARGET' requires strata-cassandra (port 9998) up. Pass --include-cassandra to auto-boot via 'make up-cassandra', or run it manually."
    fi
  fi
fi

# Lab probes — fail-soft via skip(77) unless REQUIRE_LAB=1.
if [[ "$TARGET" == "strata" || "$TARGET" == "both" || "$TARGET" == "all" ]]; then
  code="$(probe_target "$STRATA_ENDPOINT_URL")"
  if [[ "$code" != "200" && "$code" != "403" ]]; then
    if [[ "$REQUIRE_LAB" == "1" ]]; then
      fail "strata endpoint $STRATA_ENDPOINT_URL not ready (HTTP $code). Bring lab up: make up-all && make wait-strata-lab"
    else
      skip "strata endpoint $STRATA_ENDPOINT_URL not ready (HTTP $code). Run 'make up-all' first."
    fi
  fi
fi
if [[ "$TARGET" == "rgw" || "$TARGET" == "both" || "$TARGET" == "all" ]]; then
  code="$(probe_target "$RGW_ENDPOINT_URL")"
  if [[ "$code" != "200" && "$code" != "403" ]]; then
    if [[ "$REQUIRE_LAB" == "1" ]]; then
      fail "rgw endpoint $RGW_ENDPOINT_URL not ready (HTTP $code). Bring lab up: make up-bench-rgw && make wait-rgw"
    else
      skip "rgw endpoint $RGW_ENDPOINT_URL not ready (HTTP $code). Run 'make up-bench-rgw' first."
    fi
  fi
fi
if [[ "$TARGET" == "cassandra" || "$TARGET" == "all" ]]; then
  code="$(probe_target "$CASSANDRA_ENDPOINT_URL")"
  if [[ "$code" != "200" && "$code" != "403" ]]; then
    if [[ "$REQUIRE_LAB" == "1" ]]; then
      fail "cassandra endpoint $CASSANDRA_ENDPOINT_URL not ready (HTTP $code). Bring lab up: make up-cassandra && make wait-cassandra"
    else
      skip "cassandra endpoint $CASSANDRA_ENDPOINT_URL not ready (HTTP $code). Run 'make up-cassandra' or pass --include-cassandra."
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
(( INCLUDE_CASSANDRA )) && note "US-009: --include-cassandra ON (3rd target enabled; default bench is 2-target — flag adds ~50% to sweep duration)"

case "$TARGET" in
  strata|rgw|cassandra)
    dispatch_workload "$WORKLOAD" "$TARGET" "$CONCURRENCY" "$RUNS"
    ;;
  both)
    dispatch_workload "$WORKLOAD" "strata" "$CONCURRENCY" "$RUNS"
    assert_bucket_absent "strata" "$WORKLOAD"
    dispatch_workload "$WORKLOAD" "rgw" "$CONCURRENCY" "$RUNS"
    assert_bucket_absent "rgw" "$WORKLOAD"
    ;;
  all)
    # US-009: strata + rgw + cassandra in sequence. Same RADOS cluster
    # underneath all three (single-cluster bench mode in US-003 if enabled;
    # otherwise the lab's multi-cluster default applies equally to both
    # Strata flavors). Per-target assert_bucket_absent between phases catches
    # cleanup drift.
    dispatch_workload "$WORKLOAD" "strata" "$CONCURRENCY" "$RUNS"
    assert_bucket_absent "strata" "$WORKLOAD"
    dispatch_workload "$WORKLOAD" "rgw" "$CONCURRENCY" "$RUNS"
    assert_bucket_absent "rgw" "$WORKLOAD"
    dispatch_workload "$WORKLOAD" "cassandra" "$CONCURRENCY" "$RUNS"
    assert_bucket_absent "cassandra" "$WORKLOAD"
    ;;
  *)
    abort "target must be one of: strata, rgw, cassandra, both, all (got '$TARGET')"
    ;;
esac

note "done — see $RESULTS_FILE (run with --report for markdown aggregate)"
