#!/usr/bin/env bash
# scripts/check-govulncheck.sh — run govulncheck against a Go module +
# classify findings via scripts/lib/severity.sh helpers (US-001 cycle C
# supply-chain-security). Wraps the JSONL output of `govulncheck
# -format json` with a jq filter that keeps only callgraph hits whose
# last trace frame is rooted in a Strata module; promotes test-only +
# scripts/-only matches to WARN; hard-fails on any remaining HIGH hit.
#
# Usage:
#   scripts/check-govulncheck.sh             # main module
#   scripts/check-govulncheck.sh --cephimpl  # cephimpl module
#
# Env knobs:
#   GOVULNCHECK_OUTPUT_DIR — write the raw JSONL + per-axis summary here
#                           (default: stderr only).
#   GOVULNCHECK_WARN_ONLY=1 — degrade HIGH findings to WARN (smoke /
#                            recovery mode); never used in CI.
#
# Degrades to a single-line WARN + exit 0 when govulncheck is not on
# PATH — mirrors the helm-lint / promtool-check pattern so `make test`
# is not gated on the toolchain.

set -euo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)
# shellcheck source=scripts/lib/severity.sh
source "${SCRIPT_DIR}/lib/severity.sh"

axis="main"
module_dir="."
go_env_prefix=""

while (($#)); do
  case "$1" in
    --cephimpl)
      axis="cephimpl"
      module_dir="internal/data/rados/cephimpl"
      # cephimpl is a separate Go module (CLAUDE.md RADOS / cephimpl
      # module split). Run with GOWORK=off so the host's go.work, when
      # present locally, does not flip the resolved import graph.
      go_env_prefix="GOWORK=off"
      shift
      ;;
    -h|--help)
      sed -n '2,/^$/p' "$0"
      exit 0
      ;;
    *)
      echo "check-govulncheck: unknown flag: $1" >&2
      exit 2
      ;;
  esac
done

if ! command -v govulncheck >/dev/null 2>&1; then
  echo "$(severity_yellow)WARN$(severity_reset) govulncheck not installed — skip ($axis). Install: go install golang.org/x/vuln/cmd/govulncheck@latest"
  exit 0
fi

# cephimpl axis depends on librados (cgo). Skip with WARN when the
# header is absent — matches the lab's `make vet` degradation pattern
# (librados-less hosts can't link go-ceph). CI installs librados-dev so
# the scan runs.
if [[ "$axis" == "cephimpl" ]]; then
  librados_present=0
  for h in /usr/include/rados/librados.h /usr/local/include/rados/librados.h /opt/homebrew/include/rados/librados.h; do
    if [[ -f "$h" ]]; then librados_present=1; break; fi
  done
  if (( librados_present == 0 )); then
    echo "$(severity_yellow)WARN$(severity_reset) librados.h not found — skip cephimpl govulncheck. Install: apt-get install -y librados-dev (CI) / brew install ceph (macOS, lima)"
    exit 0
  fi
fi

if ! command -v jq >/dev/null 2>&1; then
  echo "$(severity_yellow)WARN$(severity_reset) jq not installed — skip ($axis). Install: apt-get install -y jq (CI) / brew install jq (macOS)"
  exit 0
fi

repo_root=$(git rev-parse --show-toplevel 2>/dev/null || pwd)
work_dir="${repo_root}/${module_dir}"

if [[ ! -d "$work_dir" ]]; then
  echo "$(severity_red)ERROR$(severity_reset) module directory not found: $work_dir" >&2
  exit 2
fi

raw_json=$(mktemp -t govulncheck-XXXXXX.json)
trap 'rm -f "$raw_json"' EXIT

echo "scanning $axis module ($module_dir) ..." >&2
# `govulncheck -format json` emits a JSONL stream of mixed record kinds
# (config, progress, osv, finding). Successful scans with zero callgraph
# hits exit 0; any callgraph hit → exit 3. Either is OK — the gate
# below decides PASS/FAIL based on the classified findings.
set +e
(
  cd "$work_dir"
  if [[ -n "$go_env_prefix" ]]; then
    eval "$go_env_prefix govulncheck -format json ./..."
  else
    govulncheck -format json ./...
  fi
) >"$raw_json" 2>/tmp/govulncheck-stderr-$$.log
gvc_exit=$?
set -e
if [[ $gvc_exit -ne 0 && $gvc_exit -ne 3 ]]; then
  cat /tmp/govulncheck-stderr-$$.log >&2 || true
  echo "$(severity_red)ERROR$(severity_reset) govulncheck failed for $axis (exit=$gvc_exit)" >&2
  rm -f /tmp/govulncheck-stderr-$$.log
  exit 1
fi
rm -f /tmp/govulncheck-stderr-$$.log

strata_modules_json=$(severity_strata_modules_jq_array)
filter=$(severity_jq_callgraph_filter)
hits_json=$(jq -s --argjson strata_modules "$strata_modules_json" "$filter" "$raw_json")
total_hits=$(jq 'length' <<<"$hits_json")

high_hits=()
warn_hits=()

if (( total_hits > 0 )); then
  while IFS= read -r row; do
    cls=$(severity_classify_hit "$row")
    if [[ "$cls" == "HIGH" ]]; then
      high_hits+=("$row")
    else
      warn_hits+=("$row")
    fi
  done < <(jq -c '.[]' <<<"$hits_json")
fi

if [[ -n "${GOVULNCHECK_WARN_ONLY:-}" ]]; then
  warn_hits+=("${high_hits[@]}")
  high_hits=()
fi

print_hit() {
  local label="$1" color="$2" payload="$3"
  jq -r --arg label "$label" --arg color "$color" --arg reset "$(severity_reset)" '
    "\($color)\($label)\($reset) \(.osv) — \(.package).\(.function)\(if (.receiver // "") != "" then "(receiver=\(.receiver))" else "" end)\(if (.fixed // "") != "" then "  fix→\(.fixed)" else "" end)"
  ' <<<"$payload"
}

if (( ${#warn_hits[@]} > 0 )); then
  echo
  echo "--- WARN ($axis): ${#warn_hits[@]} test/util-only finding(s) ---"
  for row in "${warn_hits[@]}"; do
    print_hit WARN "$(severity_yellow)" "$row"
  done
fi

if (( ${#high_hits[@]} > 0 )); then
  echo
  echo "--- HIGH ($axis): ${#high_hits[@]} callgraph finding(s) ---"
  for row in "${high_hits[@]}"; do
    print_hit HIGH "$(severity_red)" "$row"
  done
fi

if [[ -n "${GOVULNCHECK_OUTPUT_DIR:-}" ]]; then
  mkdir -p "$GOVULNCHECK_OUTPUT_DIR"
  cp "$raw_json" "${GOVULNCHECK_OUTPUT_DIR}/govulncheck-${axis}.json"
  jq -n --argjson hits "$hits_json" \
        --arg axis "$axis" \
        --argjson high "${#high_hits[@]}" \
        --argjson warn "${#warn_hits[@]}" \
        '{axis: $axis, high: $high, warn: $warn, hits: $hits}' \
        > "${GOVULNCHECK_OUTPUT_DIR}/govulncheck-${axis}-summary.json"
fi

if (( ${#high_hits[@]} > 0 )); then
  echo
  echo "$(severity_red)FAIL$(severity_reset) $axis: ${#high_hits[@]} HIGH callgraph finding(s); ${#warn_hits[@]} WARN."
  exit 1
fi

echo "$(severity_green)PASS$(severity_reset) $axis: 0 HIGH callgraph finding(s); ${#warn_hits[@]} WARN."
exit 0
