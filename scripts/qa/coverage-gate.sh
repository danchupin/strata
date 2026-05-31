#!/usr/bin/env bash
# scripts/qa/coverage-gate.sh — per-package coverage RATCHET GATE for the QA
# production-readiness cycle (US-013). Promotes the US-001 coverage baseline
# (tasks/qa-readiness-report.md §1) into a hard CI gate.
#
# Reads scripts/qa/coverage-floors.txt, measures `go test -cover` per listed
# package on the DEFAULT build tag (TiKV + memory, no ceph/librados), and FAILS
# (exit 1) when any hard-gated core package regresses below its floor. The floor
# is a ratchet: it only catches regressions below the measured baseline, it is
# never an aspirational target.
#
# CGO note (FR-4, mirrors coverage.sh): on a macOS box without the Xcode license
# accepted, the cgo runtime fails to build a handful of packages (internal/s3api,
# internal/meta/tikv). That is an ENV issue, not a code bug. Locally those
# packages are reported as "unmeasured (local cgo-blocked) — skipped" and do NOT
# fail the gate; on a Linux CI runner with a C toolchain (CI=true) every core
# package builds, so an unmeasured hard-gated package there IS a failure. CI on
# Linux is the source of truth.
#
# Usage: scripts/qa/coverage-gate.sh
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

FLOORS="scripts/qa/coverage-floors.txt"
ON_CI="${CI:-}"

# measure_pkg <import-suffix> -> echoes coverage percent (float) on success, or
# empty string when the package could not be built/measured. Tries default CGO
# first, then CGO_ENABLED=0 to recover darwin cgo-blocked packages locally.
measure_pkg() {
  local pkg="$1" out cov
  out="$(go test -cover "./${pkg}/" 2>&1)"
  cov="$(printf '%s\n' "$out" | sed -n 's/.*coverage: \([0-9.]*\)% of statements.*/\1/p' | tail -1)"
  if [[ -z "$cov" ]]; then
    out="$(CGO_ENABLED=0 go test -cover "./${pkg}/" 2>&1)"
    cov="$(printf '%s\n' "$out" | sed -n 's/.*coverage: \([0-9.]*\)% of statements.*/\1/p' | tail -1)"
  fi
  printf '%s' "$cov"
}

fail=0
echo "== QA coverage ratchet gate (default tag: TiKV + memory) =="
printf '%-28s %-10s %-10s %s\n' "PACKAGE" "MEASURED" "FLOOR" "RESULT"
printf '%-28s %-10s %-10s %s\n' "-------" "--------" "-----" "------"

while read -r pkg floor; do
  # skip blanks + comments
  [[ -z "${pkg:-}" || "${pkg:0:1}" == "#" ]] && continue

  cov="$(measure_pkg "$pkg")"

  if [[ "$floor" == "CI-ESTABLISH" ]]; then
    if [[ -z "$cov" ]]; then
      printf '%-28s %-10s %-10s %s\n' "$pkg" "n/a" "$floor" "INFO (unmeasured locally)"
    else
      printf '%-28s %-9s%% %-10s %s\n' "$pkg" "$cov" "$floor" "INFO (soft — establish baseline)"
    fi
    continue
  fi

  if [[ -z "$cov" ]]; then
    if [[ -n "$ON_CI" ]]; then
      printf '%-28s %-10s %-10s %s\n' "$pkg" "BUILD-FAIL" "${floor}%" "FAIL (unmeasured on CI)"
      fail=1
    else
      printf '%-28s %-10s %-10s %s\n' "$pkg" "n/a" "${floor}%" "skip (local cgo-blocked)"
    fi
    continue
  fi

  # float compare: measured >= floor
  if awk "BEGIN{exit !($cov >= $floor)}"; then
    printf '%-28s %-9s%% %-9s%% %s\n' "$pkg" "$cov" "$floor" "ok"
  else
    printf '%-28s %-9s%% %-9s%% %s\n' "$pkg" "$cov" "$floor" "FAIL (regressed below floor)"
    fail=1
  fi
done < "$FLOORS"

echo
if [[ "$fail" -ne 0 ]]; then
  echo "coverage gate FAILED — a core package dropped below its ratchet floor." >&2
  echo "If the drop is intentional (code removed), lower the floor in $FLOORS in the same commit." >&2
  exit 1
fi
echo "coverage gate PASSED — all core packages at or above their ratchet floors."
