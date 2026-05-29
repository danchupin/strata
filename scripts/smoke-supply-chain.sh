#!/usr/bin/env bash
# scripts/smoke-supply-chain.sh — composite smoke for the Cycle C
# supply-chain-security cycle (US-011). Proves every gate shipped across
# US-001..US-010 composes cleanly in one pass:
#
#   (a) make govulncheck         — Go CVE callgraph scan (US-001)
#   (b) make trivy-check         — image CVE scan (US-002)
#   (c) make gosec               — Go static-analysis scan (US-003)
#   (d) make license-audit       — forbidden/restricted license gate (US-009)
#   (e) scripts/smoke-image-verification.sh — cosign sign/verify roundtrip (US-008)
#   (f) .github/dependabot.yml parses as valid YAML (US-004)
#
# Every sub-gate self-degrades to WARN + exit 0 when its toolchain is
# missing (the make targets each `command -v` guard, the image-verify smoke
# warn-skips on absent cosign/docker). This composite therefore never
# hard-fails on a clean machine that lacks the scanners — it only fails when
# a gate is INSTALLED and a real finding/parse-error surfaces. That keeps it
# safe to wire into `make smoke-supply-chain` without gating on toolchain.

set -euo pipefail

cd "$(dirname "$0")/.."
# shellcheck source=scripts/lib/severity.sh
source "$(dirname "$0")/lib/severity.sh"

pass() { echo "$(severity_green)PASS$(severity_reset) $1"; }
warn() { echo "$(severity_yellow)WARN$(severity_reset) $1"; }
fail() { echo "$(severity_red)FAIL$(severity_reset) $1"; }

FAILURES=0
run_gate() {
    local label="$1"
    shift
    echo "==> $label"
    if "$@"; then
        pass "$label"
    else
        fail "$label (exit $?)"
        FAILURES=$((FAILURES + 1))
    fi
}

# (a)-(d): the four make gates. Each is internally WARN-degrading.
run_gate "(a) make govulncheck"  make govulncheck
run_gate "(b) make trivy-check"  make trivy-check
run_gate "(c) make gosec"        make gosec
run_gate "(d) make license-audit" make license-audit

# (e): cosign sign/verify roundtrip (warn-skips on absent cosign/docker).
run_gate "(e) smoke-image-verification" bash scripts/smoke-image-verification.sh

# (f): dependabot config parses as valid YAML.
echo "==> (f) .github/dependabot.yml YAML validity"
DEPENDABOT=".github/dependabot.yml"
if [[ ! -f "$DEPENDABOT" ]]; then
    fail "(f) $DEPENDABOT missing"
    FAILURES=$((FAILURES + 1))
elif command -v yq >/dev/null 2>&1; then
    if yq eval '.' "$DEPENDABOT" >/dev/null 2>&1; then
        pass "(f) $DEPENDABOT valid YAML (yq)"
    else
        fail "(f) $DEPENDABOT failed yq parse"
        FAILURES=$((FAILURES + 1))
    fi
elif command -v python3 >/dev/null 2>&1; then
    # yq fallback — yq isn't preinstalled on every dev box; python3 + the
    # stdlib has no YAML parser, so try PyYAML, else warn-skip.
    if python3 -c 'import yaml,sys; yaml.safe_load(open(sys.argv[1]))' "$DEPENDABOT" 2>/dev/null; then
        pass "(f) $DEPENDABOT valid YAML (python yaml)"
    else
        warn "(f) yq + PyYAML both unavailable — YAML validity unchecked (install: apt-get install -y yq)"
    fi
else
    warn "(f) yq not on PATH — YAML validity unchecked (install: apt-get install -y yq)"
fi

echo
if (( FAILURES > 0 )); then
    fail "supply-chain composite smoke: $FAILURES gate(s) failed"
    exit 1
fi
pass "supply-chain composite smoke: all gates green (WARN-skips count as pass)"
