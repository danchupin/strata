#!/usr/bin/env bash
# scripts/racecheck/summarize.sh — distil report/race.jsonl into report/SUMMARY.md.
#
# Acceptance (US-008):
#   - total ops by class
#   - throughput (ops/sec)
#   - p50/p95/p99 duration_ms per class
#   - inconsistency count + first 3 examples
#   - host snapshot delta (RSS pre/post, disk pre/post — from report/host.txt)
#
# Handles empty / partial reports gracefully (process killed mid-run).
# Targets <5s on a 20 MB JSON-lines report (single jq pass + sort + awk).
#
# Reads:  $REPORT_DIR/race.jsonl, $REPORT_DIR/host.txt
# Writes: $REPORT_DIR/SUMMARY.md
# Embedded into $GITHUB_STEP_SUMMARY by .github/workflows/race-nightly.yml.

set -eu

REPORT_DIR="${REPORT_DIR:-report}"
JSONL="$REPORT_DIR/race.jsonl"
HOST="$REPORT_DIR/host.txt"
OUT="$REPORT_DIR/SUMMARY.md"
TAB="$(printf '\t')"

mkdir -p "$REPORT_DIR"

placeholder() {
  {
    echo "# Race soak summary"
    echo
    echo "$1"
  } >"$OUT"
}

if ! command -v jq >/dev/null 2>&1; then
  placeholder "**\`jq\` not installed — cannot summarise.**"
  exit 0
fi

if [ ! -s "$JSONL" ]; then
  placeholder "No JSON-lines events at \`$JSONL\` (file missing or empty). The harness may have failed before emitting any ops. Check run.sh logs and per-container docker logs in \`$REPORT_DIR/\`."
  exit 0
fi

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# Single jq pass — emit one tab-separated, prefix-tagged line per relevant
# event so the rest of the script is plain awk/sort over a small temp file.
#   D<TAB>class<TAB>duration_ms          op_done
#   I<TAB>kind<TAB>bucket<TAB>key<TAB>detail   inconsistency
#   S<TAB>duration_ns                    summary
jq -r '
  if .event == "op_done" then
    "D\t\(.class // "-")\t\(.duration_ms // 0)"
  elif .event == "inconsistency" then
    .inconsistency as $i |
    "I\t\($i.kind // "-")\t\($i.bucket // "-")\t\($i.key // "-")\t\($i.detail // "")"
  elif .event == "summary" then
    "S\t\(.summary.duration_ns // 0)"
  else empty end
' "$JSONL" >"$TMP/parsed.tsv" 2>"$TMP/jq.err" || {
  placeholder "Failed to parse \`$JSONL\` (jq exited non-zero). First 20 lines of stderr:

\`\`\`
$(head -n 20 "$TMP/jq.err" 2>/dev/null || true)
\`\`\`"
  exit 0
}

# --- Latency per class: sort by class then duration ascending; awk emits
# percentile rows. Works streaming so 20 MB inputs stay under the 5s budget.
awk -F"$TAB" '$1=="D" {print $2 "\t" $3}' "$TMP/parsed.tsv" \
  | sort -t "$TAB" -k1,1 -k2,2n >"$TMP/lat.sorted"

awk -F"$TAB" '
  function emit(c, vals, n,    i50, i95, i99) {
    i50 = int(n*0.50 + 0.5); if (i50<1) i50=1; if (i50>n) i50=n
    i95 = int(n*0.95 + 0.5); if (i95<1) i95=1; if (i95>n) i95=n
    i99 = int(n*0.99 + 0.5); if (i99<1) i99=1; if (i99>n) i99=n
    printf "%s\t%d\t%s\t%s\t%s\n", c, n, vals[i50], vals[i95], vals[i99]
  }
  {
    if ($1 != cur && cur != "") {
      emit(cur, vals, n)
      delete vals
      n = 0
    }
    cur = $1
    n++
    vals[n] = $2
  }
  END { if (cur != "") emit(cur, vals, n) }
' "$TMP/lat.sorted" >"$TMP/lat.tsv"

TOTAL_OPS="$(wc -l <"$TMP/lat.sorted" | tr -d ' ')"
[ -n "$TOTAL_OPS" ] || TOTAL_OPS=0

# Duration: prefer the summary event; on partial runs (no summary) we leave
# throughput as n/a rather than guess from event timestamps — RFC3339 nano
# arithmetic in POSIX shell is more trouble than the signal it carries.
DURATION_NS="$(awk -F"$TAB" '$1=="S" {ns=$2} END {if (ns+0 > 0) print ns}' "$TMP/parsed.tsv")"
if [ -n "${DURATION_NS:-}" ]; then
  DURATION_LABEL="$(awk -v ns="$DURATION_NS" 'BEGIN{ printf "%.1fs", ns/1e9 }')"
  THROUGHPUT="$(awk -v ops="$TOTAL_OPS" -v ns="$DURATION_NS" 'BEGIN{
    if (ns+0 > 0) printf "%.1f ops/sec", ops / (ns/1e9); else print "n/a"
  }')"
else
  DURATION_LABEL="n/a"
  THROUGHPUT="n/a"
fi

# Inconsistencies — count + first 3 (acceptance criterion is exactly 3).
awk -F"$TAB" '$1=="I"' "$TMP/parsed.tsv" >"$TMP/inconsist.tsv"
INCONSIST_TOTAL="$(wc -l <"$TMP/inconsist.tsv" | tr -d ' ')"
[ -n "$INCONSIST_TOTAL" ] || INCONSIST_TOTAL=0

head -n 3 "$TMP/inconsist.tsv" \
  | awk -F"$TAB" '{ printf "- **%s** %s/%s — %s\n", $2, $3, $4, $5 }' \
  >"$TMP/inconsist.md"

# Host snapshot: extract used-disk + used-memory under a labelled section.
# Section header shape (run.sh): "== <label> <RFC3339Z>".
extract_host() {
  lbl="$1"; key="$2"
  if [ ! -f "$HOST" ]; then
    echo "-"
    return
  fi
  awk -v lbl="$lbl" -v key="$key" '
    /^== / {
      in_sec = (index($0, "== " lbl " ") == 1)
      sub_df = 0; sub_mem = 0
      next
    }
    in_sec && /^-- df / { sub_df = 1; sub_mem = 0; next }
    in_sec && /^-- free/ { sub_mem = 1; sub_df = 0; next }
    in_sec && key == "df"  && sub_df  && /\/$/ && $0 !~ /Mounted on$/ { print $3; exit }
    in_sec && key == "mem" && sub_mem && /^Mem:/ { print $3; exit }
  ' "$HOST"
}

PRE_DISK="$(extract_host pre-load df)"
POST_DISK="$(extract_host post-load df)"
PRE_MEM="$(extract_host pre-load mem)"
POST_MEM="$(extract_host post-load mem)"
[ -n "$PRE_DISK" ]  || PRE_DISK="-"
[ -n "$POST_DISK" ] || POST_DISK="-"
[ -n "$PRE_MEM" ]   || PRE_MEM="-"
[ -n "$POST_MEM" ]  || POST_MEM="-"

# --- Assemble SUMMARY.md ---
{
  echo "# Race soak summary"
  echo
  if [ "$INCONSIST_TOTAL" -gt 0 ]; then
    echo "**Status: inconsistencies detected ($INCONSIST_TOTAL).**"
  else
    echo "**Status: clean.**"
  fi
  echo
  echo "- Total ops: \`$TOTAL_OPS\`"
  echo "- Duration: \`${DURATION_LABEL}\`"
  echo "- Throughput: \`${THROUGHPUT}\`"
  echo "- Inconsistencies: \`$INCONSIST_TOTAL\`"
  echo
  echo "## Ops by class"
  echo
  if [ -s "$TMP/lat.tsv" ]; then
    echo "| Class | Count | p50 (ms) | p95 (ms) | p99 (ms) |"
    echo "|-------|------:|---------:|---------:|---------:|"
    sort -t "$TAB" -k2,2nr "$TMP/lat.tsv" \
      | awk -F"$TAB" '{ printf "| %s | %d | %s | %s | %s |\n", $1, $2, $3, $4, $5 }'
  else
    echo "_No completed ops recorded._"
  fi
  echo
  echo "## Inconsistencies"
  echo
  if [ "$INCONSIST_TOTAL" -gt 0 ]; then
    echo "First $({ [ "$INCONSIST_TOTAL" -lt 3 ] && echo "$INCONSIST_TOTAL"; } || echo 3) of $INCONSIST_TOTAL:"
    echo
    cat "$TMP/inconsist.md"
  else
    echo "_No inconsistencies detected._"
  fi
  echo
  echo "## Host snapshot"
  echo
  echo "| Metric | Pre-load | Post-load |"
  echo "|--------|----------|-----------|"
  echo "| Disk used (\`/\`) | \`$PRE_DISK\` | \`$POST_DISK\` |"
  echo "| Mem used (MiB) | \`$PRE_MEM\` | \`$POST_MEM\` |"
} >"$OUT"

printf 'wrote %s (ops=%s inconsist=%s)\n' "$OUT" "$TOTAL_OPS" "$INCONSIST_TOTAL"
