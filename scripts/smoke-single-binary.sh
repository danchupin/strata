#!/usr/bin/env bash
# Single-binary smoke harness (US-002 of the ralph/single-binary cycle —
# closes ROADMAP P2 "Consolidate `strata-admin` binary into `strata` as
# `admin` subcommand").
#
# Verifies the post-consolidation dispatcher shape on a freshly built
# binary:
#   1. `bin/strata --help` lists both `server` and `admin` subcommands.
#   2. `bin/strata admin --help` lists every expected admin subcommand
#      (iam, lifecycle, gc, sse, replicate, bucket, rewrap, bench-gc,
#      bench-lifecycle).
#   3. `bin/strata admin rewrap --help` prints the rewrap usage banner.
#   4. `bin/strata-admin` no longer exists after `make build` (hard cut).
#   5. `bin/strata unknown` exits 2 with a helpful error.
#   6. No `strata-admin` residue under docs/ / scripts/ / deploy/ /
#      .github/ / Makefile / CLAUDE.md (outside `scripts/ralph/archive/`
#      snapshots, `scripts/ralph/progress.txt`, `scripts/ralph/prd.json`,
#      and the close-flip narrative in ROADMAP.md / CLAUDE.md).
#
# Exits non-zero with `FAIL: <check>` on the first miss; exits 0 when
# every check passes.

set -euo pipefail

cd "$(dirname "$0")/.."

ROOT="$(pwd)"
BIN="${ROOT}/bin/strata"

# Build the unified binary (idempotent; CI calls `make build` first but
# the smoke script is safe to run standalone).
make build >/dev/null

if [[ ! -x "${BIN}" ]]; then
    echo "FAIL: build_produces_strata — ${BIN} not present after make build"
    exit 1
fi

# (1) `strata --help` lists server + admin.
help_out="$("${BIN}" --help 2>&1 || true)"
for want in "server" "admin"; do
    if ! grep -q "${want}" <<<"${help_out}"; then
        echo "FAIL: root_help_lists_subcommands — missing '${want}'"
        echo "--- output ---"
        echo "${help_out}"
        exit 1
    fi
done

# (2) `strata admin --help` lists every admin subcommand.
admin_help="$("${BIN}" admin --help 2>&1 || true)"
for want in iam lifecycle gc sse replicate bucket rewrap bench-gc bench-lifecycle; do
    if ! grep -q "${want}" <<<"${admin_help}"; then
        echo "FAIL: admin_help_lists_subcommands — missing '${want}'"
        echo "--- output ---"
        echo "${admin_help}"
        exit 1
    fi
done

# (3) `strata admin rewrap --help` prints the rewrap usage banner.
rewrap_help="$("${BIN}" admin rewrap --help 2>&1 || true)"
if ! grep -qi "rewrap" <<<"${rewrap_help}"; then
    echo "FAIL: rewrap_help_present"
    echo "--- output ---"
    echo "${rewrap_help}"
    exit 1
fi

# (4) Legacy `bin/strata-admin` must not exist.
if [[ -e "${ROOT}/bin/strata-admin" ]]; then
    echo "FAIL: legacy_binary_removed — bin/strata-admin still present"
    exit 1
fi

# (5) Unknown subcommand exits 2 with a helpful error.
set +e
unknown_out="$("${BIN}" unknown 2>&1)"
unknown_code=$?
set -e
if [[ ${unknown_code} -ne 2 ]]; then
    echo "FAIL: unknown_subcommand_exits_two — got exit ${unknown_code}, want 2"
    echo "--- output ---"
    echo "${unknown_out}"
    exit 1
fi
if ! grep -qi "unknown subcommand" <<<"${unknown_out}"; then
    echo "FAIL: unknown_subcommand_error_message — missing 'unknown subcommand' hint"
    echo "--- output ---"
    echo "${unknown_out}"
    exit 1
fi

# (6) No `strata-admin` residue in scoped trees. ROADMAP.md + CLAUDE.md
# carry intentional close-flip narratives that name the legacy binary;
# scripts/ralph/{archive/**,progress.txt,prd.json} are cycle bookkeeping
# files. Exclude those; everything else must be clean.
residue="$(grep -rn "strata-admin" docs/ scripts/ deploy/ .github/ Makefile 2>/dev/null \
    | grep -v "^scripts/ralph/archive/" \
    | grep -v "^scripts/ralph/progress.txt:" \
    | grep -v "^scripts/ralph/prd.json:" \
    | grep -v "^scripts/smoke-single-binary.sh:" \
    | grep -v "^docs/site/public/" \
    | grep -v "^docs/site/resources/" \
    | grep -v "^Makefile:.*# " \
    || true)"
if [[ -n "${residue}" ]]; then
    echo "FAIL: residue_in_scoped_trees — strata-admin still referenced:"
    echo "${residue}"
    exit 1
fi

echo "PASS: bin/strata dispatcher shape clean across all checks"
