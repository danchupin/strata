#!/usr/bin/env bash
# scripts/lib/severity.sh — shared severity-classification helpers used by
# the supply-chain CI gates (US-001 govulncheck, US-002 trivy, US-003
# gosec, US-009 go-licenses). Source from a wrapper script:
#
#   source "$(dirname "$0")/lib/severity.sh"
#
# The helpers are pure bash + jq; no Go / Python deps. Mirrors the
# `helm-lint` / `promtool-check` degradation pattern — wrappers detect a
# missing binary and exit 0 with a WARN, so `make test` is not gated on
# the toolchain.

set -u

# Module + package prefix that defines "Strata code" for callgraph
# classification. cephimpl is a separate Go module per CLAUDE.md
# RADOS / cephimpl module split — both axes count.
SEVERITY_STRATA_MODULES_DEFAULT=(
  "github.com/danchupin/strata"
  "github.com/danchupin/strata/cephimpl"
)

# Package suffix / substring patterns that promote a callgraph hit to
# WARN instead of HARD-FAIL. Order matters — first match wins.
#   - internal/racetest: standalone race harness, not the gateway hot path
#   - scripts/:         developer/operator one-shot utilities
#   - cmd/strata-racecheck: companion binary built from racetest
SEVERITY_WARN_PATTERNS_DEFAULT=(
  "/internal/racetest"
  "/scripts/"
  "/cmd/strata-racecheck"
)

# severity_color CODE → ANSI escape; falls back to plain text when
# stdout is not a TTY or NO_COLOR is set.
severity_color() {
  local code="$1"
  if [[ -n "${NO_COLOR:-}" ]] || ! [ -t 1 ]; then
    return 0
  fi
  printf '\033[%sm' "$code"
}

severity_red()    { severity_color '0;31'; }
severity_yellow() { severity_color '0;33'; }
severity_green()  { severity_color '0;32'; }
severity_reset()  { severity_color '0'; }

# severity_jq_callgraph_filter expands to a jq program that emits
# one object per unique (osv, package, function) callgraph hit whose
# last trace frame's module matches a Strata module. Stdin is the
# govulncheck JSONL stream slurped via `jq -s`.
severity_jq_callgraph_filter() {
  cat <<'JQ'
map(select(.finding and (.finding.trace | length > 1)))
| map(select(.finding.trace[-1].module as $m
              | $strata_modules | index($m)))
| map({
    osv:      .finding.osv,
    fixed:    (.finding.fixed_version // ""),
    module:   .finding.trace[-1].module,
    package:  (.finding.trace[-1].package // ""),
    function: (.finding.trace[-1].function // ""),
    receiver: (.finding.trace[-1].receiver // ""),
    file:     ((.finding.trace[-1].position.filename) // ""),
  })
| unique_by(.osv + "::" + .package + "::" + .function + "::" + .receiver)
JQ
}

# severity_classify_hit OBJECT → echoes "HIGH" or "WARN".
# A WARN_PATTERNS match → WARN; otherwise HIGH (govulncheck's curated
# Go vuln DB already filters to reachable+exploitable findings, so any
# callgraph hit in Strata code is treated as ≥HIGH by default).
severity_classify_hit() {
  local payload="$1"
  local pkg
  pkg=$(jq -r '.package // ""' <<<"$payload")
  local file
  file=$(jq -r '.file // ""' <<<"$payload")
  local pattern
  for pattern in "${SEVERITY_WARN_PATTERNS[@]}"; do
    if [[ "$pkg" == *"$pattern"* ]] || [[ "$file" == *"$pattern"* ]]; then
      echo "WARN"
      return 0
    fi
  done
  if [[ "$file" == *"_test.go" ]]; then
    echo "WARN"
    return 0
  fi
  echo "HIGH"
}

# severity_strata_modules_jq_array → JSON array literal of Strata
# module names for embedding into a jq --argjson invocation.
severity_strata_modules_jq_array() {
  printf '['
  local first=1 m
  for m in "${SEVERITY_STRATA_MODULES[@]}"; do
    if (( first )); then first=0; else printf ','; fi
    printf '"%s"' "$m"
  done
  printf ']'
}

# Defaults — wrappers can override by reassigning SEVERITY_STRATA_MODULES
# or SEVERITY_WARN_PATTERNS BEFORE calling any helper.
: "${SEVERITY_STRATA_MODULES:=}"
: "${SEVERITY_WARN_PATTERNS:=}"
if [[ -z "${SEVERITY_STRATA_MODULES[*]:-}" ]]; then
  SEVERITY_STRATA_MODULES=("${SEVERITY_STRATA_MODULES_DEFAULT[@]}")
fi
if [[ -z "${SEVERITY_WARN_PATTERNS[*]:-}" ]]; then
  SEVERITY_WARN_PATTERNS=("${SEVERITY_WARN_PATTERNS_DEFAULT[@]}")
fi
