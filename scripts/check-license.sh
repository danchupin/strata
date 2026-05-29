#!/usr/bin/env bash
# scripts/check-license.sh — run go-licenses against a Go module +
# fail on forbidden/restricted license classes (US-009 cycle C
# supply-chain-security).
#
# Gate semantics (per Google go-licenses taxonomy):
#   forbidden  → hard-fail (GPL, AGPL, ...).
#   restricted → hard-fail (LGPL strong-copyleft, ...).
#   reciprocal (MPL-2.0), notice (MIT/BSD/Apache), permissive,
#              unencumbered → allowed.
# An UNKNOWN license also hard-fails unless the module is listed under
# `ignore:` in .licensei.yml (manually-verified allowed class whose
# LICENSE text scores below the classifier confidence threshold).
#
# Usage:
#   scripts/check-license.sh             # main module
#   scripts/check-license.sh --cephimpl  # cephimpl module
#
# Env knobs:
#   GOLICENSES_OUTPUT_DIR — write license-report.csv per axis here
#                           (default: stderr only).
#
# Degrades to a single-line WARN + exit 0 when go-licenses is not on
# PATH — mirrors the helm-lint / promtool-check / govulncheck /
# trivy-check / gosec wrapper pattern so `make test` is not gated on
# the toolchain.

set -euo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)
# shellcheck source=scripts/lib/severity.sh
source "${SCRIPT_DIR}/lib/severity.sh"

axis="main"
module_dir="."

while (($#)); do
  case "$1" in
    --cephimpl)
      axis="cephimpl"
      module_dir="internal/data/rados/cephimpl"
      shift
      ;;
    -h|--help)
      sed -n '2,/^$/p' "$0"
      exit 0
      ;;
    *)
      echo "check-license: unknown flag: $1" >&2
      exit 2
      ;;
  esac
done

if ! command -v go-licenses >/dev/null 2>&1; then
  echo "$(severity_yellow)WARN$(severity_reset) go-licenses not installed — skip ($axis). Install: go install github.com/google/go-licenses@v1.6.0"
  exit 0
fi

repo_root=$(git rev-parse --show-toplevel 2>/dev/null || pwd)
work_dir="${repo_root}/${module_dir}"

if [[ ! -d "$work_dir" ]]; then
  echo "$(severity_red)ERROR$(severity_reset) module directory not found: $work_dir" >&2
  exit 2
fi

# Parse the ignore allowlist from .licensei.yml: every `- <module>`
# line under the `ignore:` key. The trailing `# <license> — <reason>`
# comment is for humans; we keep only the first token.
ignore_args=()
licensei="${repo_root}/.licensei.yml"
if [[ -f "$licensei" ]]; then
  in_ignore=0
  while IFS= read -r line; do
    if [[ "$line" =~ ^ignore:[[:space:]]*$ ]]; then
      in_ignore=1
      continue
    fi
    # A new top-level key (no leading whitespace, ends with ':') closes
    # the ignore block.
    if (( in_ignore )) && [[ "$line" =~ ^[^[:space:]#].*:[[:space:]]*$ ]]; then
      in_ignore=0
    fi
    if (( in_ignore )) && [[ "$line" =~ ^[[:space:]]*-[[:space:]]+([^[:space:]#]+) ]]; then
      ignore_args+=(--ignore "${BASH_REMATCH[1]}")
    fi
  done < "$licensei"
fi

# go-licenses audits each module independently (CLAUDE.md RADOS /
# cephimpl module split), so the workspace is turned OFF. Run in the
# default readonly module mode: go-licenses v1.6.0 mis-reports stdlib
# packages as "Non go modules projects" under GOFLAGS=-mod=mod (issue
# #128), and the main module's go.sum is complete on its own (the
# go.work.sum-only x/sync hash was backfilled in US-009).
export GOWORK=off

# Strip the workspace-only WARN/non-Go-code noise lines from go-licenses
# stderr so a real disallowed-license error stands out. Keep E/F lines.
run_go_licenses() {
  local verb="$1"; shift
  (cd "$work_dir" && go-licenses "$verb" ./... "${ignore_args[@]}" "$@")
}

echo "auditing $axis module ($module_dir) for forbidden/restricted licenses ..." >&2

set +e
check_out=$(run_go_licenses check --disallowed_types=forbidden,restricted 2>&1)
check_exit=$?
set -e

# Drop the benign noise: non-Go-code WARNs + the file-path continuation
# lines they emit.
filtered=$(printf '%s\n' "$check_out" \
  | grep -vE '^W[0-9]|contains non-Go code|^[[:space:]]*/.*\.(s|c|h|cpp|cc)$' || true)

if [[ -n "${GOLICENSES_OUTPUT_DIR:-}" ]]; then
  mkdir -p "$GOLICENSES_OUTPUT_DIR"
  # Operator-facing license inventory. report can exit non-zero on the
  # same unknown-license rows the ignore list covers — best-effort, the
  # check gate above is authoritative.
  set +e
  run_go_licenses report 2>/dev/null > "${GOLICENSES_OUTPUT_DIR}/license-report-${axis}.csv"
  set -e
fi

if (( check_exit != 0 )); then
  echo
  echo "$(severity_red)FAIL$(severity_reset) $axis: go-licenses check reported disallowed/unknown license(s):"
  printf '%s\n' "$filtered" >&2
  echo
  echo "If the offending module carries a verified ALLOWED license that the" >&2
  echo "classifier mis-scores as unknown, add it under ignore: in .licensei.yml" >&2
  echo "with a rationale comment. NEVER ignore a real GPL/AGPL/LGPL module." >&2
  exit 1
fi

echo "$(severity_green)PASS$(severity_reset) $axis: 0 forbidden/restricted license(s); $(( ${#ignore_args[@]} / 2 )) ignored module(s) (see .licensei.yml)."
exit 0
