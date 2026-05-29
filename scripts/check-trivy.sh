#!/usr/bin/env bash
# scripts/check-trivy.sh — run trivy image scan against the locally-built
# strata:ceph image + classify findings via scripts/lib/severity.sh helpers
# (US-002 cycle C supply-chain-security).
#
# Gate semantics:
#   CRITICAL → hard-fail (exit 1) — matches the CI gate at
#              .github/workflows/ci.yml :: trivy-image (CRITICAL pass).
#   HIGH     → WARN-only (exit 0) — matches the CI HIGH pass which
#              uploads JSON artifact for review.
#
# Usage:
#   scripts/check-trivy.sh                # default: scan strata:ceph CRITICAL
#   scripts/check-trivy.sh --image=NAME   # alternate image ref
#   scripts/check-trivy.sh --high         # WARN-only HIGH pass
#
# Degrades to a single-line WARN + exit 0 when trivy is not on PATH or
# the target image is not present locally — mirrors the helm-lint /
# promtool-check / govulncheck wrapper pattern so `make test` is not
# gated on the toolchain. Operators run `make docker-build` first to
# populate strata:ceph before this script does real work.
#
# Env knobs:
#   TRIVY_OUTPUT_DIR — write raw JSON output here (default: stderr only).
#   TRIVY_CONFIG    — alternate trivy config path (default: trivy.yaml at
#                     repo root).

set -euo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)
# shellcheck source=scripts/lib/severity.sh
source "${SCRIPT_DIR}/lib/severity.sh"

image_ref="strata:ceph"
severity="CRITICAL"
exit_on_finding=1

while (($#)); do
  case "$1" in
    --image=*)
      image_ref="${1#*=}"
      shift
      ;;
    --high)
      severity="HIGH"
      exit_on_finding=0
      shift
      ;;
    -h|--help)
      sed -n '2,/^$/p' "$0"
      exit 0
      ;;
    *)
      echo "check-trivy: unknown flag: $1" >&2
      exit 2
      ;;
  esac
done

if ! command -v trivy >/dev/null 2>&1; then
  echo "$(severity_yellow)WARN$(severity_reset) trivy not installed — skip. Install: https://aquasecurity.github.io/trivy/latest/getting-started/installation/"
  exit 0
fi

if ! command -v docker >/dev/null 2>&1; then
  echo "$(severity_yellow)WARN$(severity_reset) docker not on PATH — skip trivy-check (cannot resolve $image_ref)"
  exit 0
fi

if ! docker image inspect "$image_ref" >/dev/null 2>&1; then
  echo "$(severity_yellow)WARN$(severity_reset) image $image_ref not present locally — skip. Build first: make docker-build"
  exit 0
fi

repo_root=$(git rev-parse --show-toplevel 2>/dev/null || pwd)
trivy_config="${TRIVY_CONFIG:-${repo_root}/trivy.yaml}"

output_dir="${TRIVY_OUTPUT_DIR:-}"
if [[ -n "$output_dir" ]]; then
  mkdir -p "$output_dir"
fi

json_out=$(mktemp -t trivy-XXXXXX.json)
trap 'rm -f "$json_out"' EXIT

echo "scanning $image_ref severity=$severity ..." >&2
set +e
trivy image \
  --config "$trivy_config" \
  --severity "$severity" \
  --format json \
  --output "$json_out" \
  "$image_ref"
scan_exit=$?
set -e

if (( scan_exit != 0 )); then
  echo "$(severity_red)ERROR$(severity_reset) trivy scan failed (exit=$scan_exit)" >&2
  exit 1
fi

# Trivy's --format json emits an object with `.Results[].Vulnerabilities[]`
# entries — sum across the layers.
total=$(jq '[.Results[]?.Vulnerabilities // [] | length] | add // 0' "$json_out")

if [[ -n "$output_dir" ]]; then
  cp "$json_out" "${output_dir}/trivy-${severity,,}.json"
fi

if (( total == 0 )); then
  echo "$(severity_green)PASS$(severity_reset) trivy $severity: 0 findings in $image_ref"
  exit 0
fi

if (( exit_on_finding == 0 )); then
  echo "$(severity_yellow)WARN$(severity_reset) trivy $severity: $total finding(s) in $image_ref (WARN-only pass)"
  jq -r '.Results[]? | .Target as $t | .Vulnerabilities // [] | .[] | "  \($t)  \(.VulnerabilityID)  \(.PkgName)@\(.InstalledVersion)\(if .FixedVersion != "" and .FixedVersion != null then "  fix→\(.FixedVersion)" else "" end)"' "$json_out"
  exit 0
fi

echo "$(severity_red)FAIL$(severity_reset) trivy $severity: $total finding(s) in $image_ref"
jq -r '.Results[]? | .Target as $t | .Vulnerabilities // [] | .[] | "  \($t)  \(.VulnerabilityID)  \(.PkgName)@\(.InstalledVersion)\(if .FixedVersion != "" and .FixedVersion != null then "  fix→\(.FixedVersion)" else "" end)"' "$json_out"
exit 1
