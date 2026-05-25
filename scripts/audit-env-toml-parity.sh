#!/usr/bin/env bash
# audit-env-toml-parity.sh
#
# Produces a deterministic gap-list of every STRATA_* environment variable
# read by main-module code vs the central envMap registry (internal/config/
# config.go), the Config struct's koanf tags, and deploy/strata.toml.example.
#
# Outputs:
#   scripts/audit-results/env-toml-parity-<date>.jsonl  -- one row per env var
#   stdout summary table (totals + percentages)
#
# Consumed by:
#   - US-001 gap-list generation (tasks/toml-parity-gaps.md)
#   - US-006 drift-lint test (internal/config/env_toml_parity_test.go)
#
# Naming convention 2A (PRD-zashited):
#   STRATA_<SECTION>_<KNOB> -> <section>.<knob> (lowercase)
#   First underscore splits section; remaining underscores stay snake_case in
#   the knob. Multi-word sections (e.g. usage_rollup, manifest_rewriter,
#   audit_export) MUST be declared explicitly in envMap so the heuristic
#   does not mis-classify them.

set -euo pipefail

ROOT="$(git rev-parse --show-toplevel)"
cd "$ROOT"

CONFIG_GO="internal/config/config.go"
EXEMPT_GO="internal/config/exempt_env_vars.go"
TOML_EXAMPLE="deploy/strata.toml.example"
OUTDIR="scripts/audit-results"
mkdir -p "$OUTDIR"
DATE="$(date +%Y-%m-%d)"
JSONL="$OUTDIR/env-toml-parity-$DATE.jsonl"

command -v jq >/dev/null || { echo "audit: jq required" >&2; exit 1; }

# --- 1. Env var set: capture STRATA_* names passed as function args ------------
# (matches os.Getenv("STRATA_X"), os.Setenv, t.Setenv, envInt, envBool,
# envDuration, envOrDefault, ... and the envMap key literals). Filters out
# bare string literals used as data (e.g. lifecycle storage-class identifiers
# like "STRATA_COLD") which never appear inside a function call.
ENV_VARS_RAW="$(mktemp)"
trap 'rm -f "$ENV_VARS_RAW" "$ENVMAP_TSV" "$TOML_KEYS" "$EXEMPT_EXACT" "$EXEMPT_PREFIX" "$EXEMPT_SUFFIX"' EXIT

# --- 2. envMap (env -> TOML key) from config.go. ---------------------------------
ENVMAP_TSV="$(mktemp)"
awk '/^var envMap = map\[string\]string{/,/^}/' "$CONFIG_GO" \
  | grep -Eo '"STRATA_[A-Z0-9_]+":[[:space:]]*"[a-z0-9_.]+"' \
  | sed -E 's/"([^"]+)":[[:space:]]*"([^"]+)"/\1\t\2/' \
  | sort -u >"$ENVMAP_TSV"

{
  # Function-call args: os.Getenv("STRATA_X"), envInt("STRATA_X"), ...
  grep -rhoE '\("STRATA_[A-Z0-9_]+"' cmd/ internal/ \
    | grep -Eo 'STRATA_[A-Z0-9_]+'
  # Const / var declarations: EnvFoo = "STRATA_X" — env vars read via a
  # named constant don't show up at the call site.
  grep -rhoE '=[[:space:]]+"STRATA_[A-Z0-9_]+"' cmd/ internal/ \
    | grep -Eo 'STRATA_[A-Z0-9_]+'
  awk -F'\t' '{print $1}' "$ENVMAP_TSV"
} | grep -vE '_$' | sort -u >"$ENV_VARS_RAW"

envmap_value_for() {
  awk -F'\t' -v k="$1" '$1==k {print $2; exit}' "$ENVMAP_TSV"
}

# --- 3. TOML keys present in deploy/strata.toml.example (both live + commented). -
TOML_KEYS="$(mktemp)"
awk '
  /^\[[a-z_][a-z0-9_.]*\]/ {
    s=$0; gsub(/\[|\]/, "", s); section=s; next
  }
  /^[[:space:]]*#?[[:space:]]*[a-z_][a-z0-9_]*[[:space:]]*=/ {
    line=$0
    sub(/^[[:space:]]*#?[[:space:]]*/, "", line)
    split(line, a, "=")
    key=a[1]
    gsub(/[[:space:]]/, "", key)
    if (key ~ /^[a-z_][a-z0-9_]*$/) {
      if (section != "") print section "." key
      else print key
    }
  }
' "$TOML_EXAMPLE" | sort -u >"$TOML_KEYS"

toml_has() {
  grep -qFx "$1" "$TOML_KEYS"
}

# --- 4. Exempt slices parsed from internal/config/exempt_env_vars.go. -----------
EXEMPT_EXACT="$(mktemp)"
EXEMPT_PREFIX="$(mktemp)"
EXEMPT_SUFFIX="$(mktemp)"
awk '/Exact:[[:space:]]*\[\]string{/,/},/' "$EXEMPT_GO" \
  | grep -Eo '"STRATA_[A-Z0-9_]+"' | tr -d '"' | sort -u >"$EXEMPT_EXACT"
awk '/Prefixes:[[:space:]]*\[\]string{/,/},/' "$EXEMPT_GO" \
  | grep -Eo '"STRATA_[A-Z0-9_]+"' | tr -d '"' | sort -u >"$EXEMPT_PREFIX"
awk '/Suffixes:[[:space:]]*\[\]string{/,/},/' "$EXEMPT_GO" \
  | grep -Eo '"_[A-Z0-9_]+"' | tr -d '"' | sort -u >"$EXEMPT_SUFFIX"

is_exempt() {
  local v="$1"
  grep -qFx "$v" "$EXEMPT_EXACT" && return 0
  while read -r p; do [[ -z "$p" ]] && continue; [[ "$v" == "$p"* ]] && return 0; done <"$EXEMPT_PREFIX"
  while read -r s; do [[ -z "$s" ]] && continue; [[ "$v" == *"$s" ]] && return 0; done <"$EXEMPT_SUFFIX"
  return 1
}

# --- 5. Heuristic: first-underscore split when env is NOT in envMap. ------------
guess_toml_key() {
  local env="$1"
  local stripped="${env#STRATA_}"
  local lower
  lower="$(printf '%s' "$stripped" | tr '[:upper:]' '[:lower:]')"
  if [[ "$lower" == *_* ]]; then
    printf '%s.%s\n' "${lower%%_*}" "${lower#*_}"
  else
    printf '%s\n' "$lower"
  fi
}

# --- 6. Emit jsonl + summary. ----------------------------------------------------
> "$JSONL"
TOTAL=0; IN_ENVMAP=0; EXEMPT=0; IN_EXAMPLE=0; UNMAPPED=0

while read -r env; do
  [[ -z "$env" ]] && continue
  TOTAL=$((TOTAL+1))

  expected="$(envmap_value_for "$env")"
  in_envmap=false
  if [[ -n "$expected" ]]; then
    in_envmap=true
    IN_ENVMAP=$((IN_ENVMAP+1))
  else
    expected="$(guess_toml_key "$env")"
  fi

  exempt=false
  if is_exempt "$env"; then
    exempt=true
    EXEMPT=$((EXEMPT+1))
  fi

  in_example=false
  if toml_has "$expected"; then
    in_example=true
    IN_EXAMPLE=$((IN_EXAMPLE+1))
  fi

  if [[ "$in_envmap" == false && "$exempt" == false ]]; then
    UNMAPPED=$((UNMAPPED+1))
  fi

  jq -nc \
    --arg env "$env" \
    --argjson in_envmap "$in_envmap" \
    --arg expected "$expected" \
    --argjson in_example "$in_example" \
    --argjson exempt "$exempt" \
    '{env_var: $env, in_env_map: $in_envmap, expected_toml_key: $expected, present_in_example: $in_example, exempt: $exempt}' \
    >>"$JSONL"
done <"$ENV_VARS_RAW"

pct() {
  local n="$1" d="$2"
  if [[ "$d" -eq 0 ]]; then printf '0.0%%'; return; fi
  awk -v n="$n" -v d="$d" 'BEGIN { printf "%.1f%%", (n/d)*100 }'
}

echo ""
echo "=== TOML parity audit ==="
echo "Date:                $DATE"
echo "Output:              $JSONL"
echo ""
printf '%-32s %10s\n' "Total STRATA_* env vars" "$TOTAL"
printf '%-32s %10s  %s\n' "In envMap (wired)"      "$IN_ENVMAP"  "$(pct "$IN_ENVMAP" "$TOTAL")"
printf '%-32s %10s  %s\n' "Exempt"                 "$EXEMPT"     "$(pct "$EXEMPT" "$TOTAL")"
printf '%-32s %10s  %s\n' "Present in TOML example" "$IN_EXAMPLE" "$(pct "$IN_EXAMPLE" "$TOTAL")"
printf '%-32s %10s  %s\n' "Unmapped (gap)"         "$UNMAPPED"   "$(pct "$UNMAPPED" "$TOTAL")"
echo ""
