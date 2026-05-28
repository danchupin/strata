#!/usr/bin/env bash
# scripts/check-gosec.sh — run gosec against the main Go module +
# classify findings via scripts/lib/severity.sh helpers (US-003 cycle
# C supply-chain-security).
#
# Gate semantics:
#   MEDIUM+ → hard-fail (exit 1) — matches the CI gate at
#             .github/workflows/ci.yml :: gosec (gate pass).
#   LOW     → WARN-only (exit 0) — matches the CI LOW pass which
#             uploads JSON artifact for review.
#
# Usage:
#   scripts/check-gosec.sh                # default: MEDIUM+ gate
#   scripts/check-gosec.sh --low          # WARN-only LOW pass
#
# Degrades to a single-line WARN + exit 0 when gosec is not on PATH —
# mirrors the helm-lint / promtool-check / govulncheck / trivy-check
# wrapper pattern so `make test` is not gated on the toolchain.
#
# Env knobs:
#   GOSEC_OUTPUT_DIR — write raw JSON / SARIF reports here (default:
#                      stderr only).
#
# Rule + path excludes — keep this list in lockstep with .gosec.yml
# rationale + the workflow's `-exclude=...` invocation. See .gosec.yml
# for the per-rule rationale.

set -euo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)
# shellcheck source=scripts/lib/severity.sh
source "${SCRIPT_DIR}/lib/severity.sh"

# Rule excludes — see .gosec.yml for rationale per rule.
GOSEC_EXCLUDE_RULES="G101,G104,G115,G304,G401,G501,G505,G703,G704,G705,G709"
# Path excludes — race-test harness + dev one-shot scripts.
GOSEC_EXCLUDE_DIRS=(--exclude-dir=internal/racetest --exclude-dir=scripts)

severity="medium"
exit_on_finding=1

while (($#)); do
  case "$1" in
    --low)
      severity="low"
      exit_on_finding=0
      shift
      ;;
    -h|--help)
      sed -n '2,/^$/p' "$0"
      exit 0
      ;;
    *)
      echo "check-gosec: unknown flag: $1" >&2
      exit 2
      ;;
  esac
done

if ! command -v gosec >/dev/null 2>&1; then
  echo "$(severity_yellow)WARN$(severity_reset) gosec not installed — skip. Install: go install github.com/securego/gosec/v2/cmd/gosec@latest"
  exit 0
fi

if ! command -v jq >/dev/null 2>&1; then
  echo "$(severity_yellow)WARN$(severity_reset) jq not installed — skip. Install: apt-get install -y jq (CI) / brew install jq (macOS)"
  exit 0
fi

output_dir="${GOSEC_OUTPUT_DIR:-}"
if [[ -n "$output_dir" ]]; then
  mkdir -p "$output_dir"
fi

json_out=$(mktemp -t gosec-XXXXXX.json)
trap 'rm -f "$json_out"' EXIT

echo "scanning main module severity=$severity exclude=$GOSEC_EXCLUDE_RULES ..." >&2
set +e
gosec \
  -severity="$severity" \
  -exclude="$GOSEC_EXCLUDE_RULES" \
  "${GOSEC_EXCLUDE_DIRS[@]}" \
  -fmt=json \
  -out="$json_out" \
  ./... 2>/tmp/gosec-stderr-$$.log
scan_exit=$?
set -e

# gosec exit codes: 0 = no findings; 1 = findings found; 2 = scan
# error. The wrapper accepts {0, 1} and decides PASS/FAIL via the JSON
# issue count + the `exit_on_finding` flag.
if (( scan_exit != 0 && scan_exit != 1 )); then
  cat /tmp/gosec-stderr-$$.log >&2 || true
  echo "$(severity_red)ERROR$(severity_reset) gosec scan failed (exit=$scan_exit)" >&2
  rm -f /tmp/gosec-stderr-$$.log
  exit 1
fi
rm -f /tmp/gosec-stderr-$$.log

total=$(jq '.Issues | length' "$json_out")

if [[ -n "$output_dir" ]]; then
  cp "$json_out" "${output_dir}/gosec-${severity}.json"
fi

if (( total == 0 )); then
  echo "$(severity_green)PASS$(severity_reset) gosec $severity: 0 finding(s); $(jq -r '.Stats.nosec' "$json_out") nosec annotation(s)."
  exit 0
fi

if (( exit_on_finding == 0 )); then
  echo "$(severity_yellow)WARN$(severity_reset) gosec $severity: $total finding(s) (WARN-only pass)"
  jq -r '.Issues[] | "  \(.severity)  \(.rule_id)  \(.file):\(.line)  \(.details)"' "$json_out"
  exit 0
fi

echo "$(severity_red)FAIL$(severity_reset) gosec $severity: $total finding(s)"
jq -r '.Issues[] | "  \(.severity)  \(.rule_id)  \(.file):\(.line)  \(.details)"' "$json_out"
exit 1
