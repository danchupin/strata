#!/usr/bin/env bash
# US-005 of ralph/auth-dx-trailer-lima — generate the auto-managed RGW
# comparison doc block from the latest scripts/bench-results/rgw-comparison-
# *.jsonl, and emit a regression number for the workflow's auto-merge gate.
#
# Invariants:
#   - Idempotent: running twice against the same jsonl produces identical doc
#     content and identical baseline file.
#   - Doc edits stay between the BENCH-AUTO-START / BENCH-AUTO-END markers in
#     docs/site/content/architecture/benchmarks/rgw-comparison.md. Operator-
#     curated tables outside the markers are not touched.
#   - Baseline JSON is committed
#     (docs/site/content/architecture/benchmarks/data/rgw-bench-baseline.json)
#     so the next workflow run has a numerical anchor for regression diff.
#
# Usage:
#   scripts/bench-update-doc.sh             # reads latest jsonl, writes doc + baseline, prints regression
#   scripts/bench-update-doc.sh --check     # verify doc matches latest jsonl; fail if drift (dry-run for CI)
#
# Env knobs:
#   BENCH_RESULTS_DIR     default scripts/bench-results
#   BENCH_DOC_FILE        default docs/site/content/architecture/benchmarks/rgw-comparison.md
#   BENCH_BASELINE_FILE   default docs/site/content/architecture/benchmarks/data/rgw-bench-baseline.json
#   BENCH_RUNNER_INFO     optional pre-rendered "spec block" injected verbatim into the doc
#                         (CPU/RAM/disk lines collected by the workflow). Multiline OK.
#   BENCH_COMMIT_SHA      Strata commit SHA stamped into the auto block (default `git rev-parse HEAD`).
#   BENCH_RGW_IMAGE       RGW image tag stamped into the auto block (default quay.io/ceph/ceph:v19.2.3).
#   GITHUB_OUTPUT         if set, the script also appends `max_regression_pct=<N>` and
#                         `changed=<true|false>` for the workflow to consume.

set -euo pipefail

BENCH_RESULTS_DIR="${BENCH_RESULTS_DIR:-scripts/bench-results}"
BENCH_DOC_FILE="${BENCH_DOC_FILE:-docs/site/content/architecture/benchmarks/rgw-comparison.md}"
BENCH_BASELINE_FILE="${BENCH_BASELINE_FILE:-docs/site/content/architecture/benchmarks/data/rgw-bench-baseline.json}"
BENCH_COMMIT_SHA="${BENCH_COMMIT_SHA:-$(git rev-parse HEAD 2>/dev/null || echo unknown)}"
BENCH_RGW_IMAGE="${BENCH_RGW_IMAGE:-quay.io/ceph/ceph:v19.2.3}"
BENCH_RUNNER_INFO_RAW="${BENCH_RUNNER_INFO:-}"

MODE="update"
case "${1:-}" in
  --check) MODE="check" ;;
  --help|-h) sed -n '2,32p' "$0"; exit 0 ;;
  "") ;;
  *) echo "FAIL: unknown arg: $1" >&2; exit 2 ;;
esac

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

if [[ ! -f "$BENCH_DOC_FILE" ]]; then
  echo "FAIL: BENCH_DOC_FILE missing: $BENCH_DOC_FILE" >&2
  exit 2
fi
mkdir -p "$(dirname "$BENCH_BASELINE_FILE")"

# Locate the most-recent jsonl (lexical sort of YYYY-MM-DD-suffixed files).
latest_jsonl="$(ls -1 "$BENCH_RESULTS_DIR"/rgw-comparison-*.jsonl 2>/dev/null \
  | sort | tail -1 || true)"
if [[ -z "$latest_jsonl" || ! -s "$latest_jsonl" ]]; then
  echo "FAIL: no rgw-comparison-*.jsonl under $BENCH_RESULTS_DIR" >&2
  exit 2
fi

if ! command -v python3 >/dev/null 2>&1; then
  echo "FAIL: python3 required" >&2
  exit 2
fi

# Generate doc block + baseline + regression via embedded python — keeps
# deterministic sort + float formatting in one place.
generated_block="$(BENCH_RUNNER_INFO_RAW="$BENCH_RUNNER_INFO_RAW" python3 - \
  "$latest_jsonl" "$BENCH_BASELINE_FILE" "$BENCH_COMMIT_SHA" "$BENCH_RGW_IMAGE" <<'PY'
import json
import os
import sys
from collections import defaultdict
from datetime import datetime, timezone
from statistics import median

jsonl_path, baseline_path, commit_sha, rgw_image = sys.argv[1:5]
runner_info = os.environ.get("BENCH_RUNNER_INFO_RAW", "").strip()
# Date is derived from the JSONL filename (rgw-comparison-YYYY-MM-DD.jsonl),
# not `datetime.now()`, so repeated runs against the same JSONL produce
# byte-identical doc bytes (idempotence requirement).
basename = os.path.basename(jsonl_path)
if basename.startswith("rgw-comparison-") and basename.endswith(".jsonl"):
    refresh_date = basename[len("rgw-comparison-"):-len(".jsonl")]
else:
    refresh_date = datetime.now(timezone.utc).strftime("%Y-%m-%d")

rows = []
with open(jsonl_path) as fh:
    for line in fh:
        line = line.strip()
        if not line:
            continue
        try:
            rows.append(json.loads(line))
        except json.JSONDecodeError:
            continue

# Aggregate runs per (target, workload, object_size, concurrency) — median p99
# is the headline; median p50/throughput preserved as context.
groups = defaultdict(list)
for r in rows:
    key = (r["target"], r["workload"], r.get("object_size", ""), int(r["concurrency"]))
    groups[key].append(r)

agg = {}
for key, runs in groups.items():
    p50 = median(float(r.get("p50_ms", 0)) for r in runs)
    p95 = median(float(r.get("p95_ms", 0)) for r in runs)
    p99 = median(float(r.get("p99_ms", 0)) for r in runs)
    mbps = median(float(r.get("throughput_mbps", 0)) for r in runs)
    ops = median(float(r.get("throughput_ops_per_sec", 0)) for r in runs)
    errs = sum(int(r.get("errors", 0)) for r in runs)
    agg[key] = {
        "p50_ms": round(p50, 3),
        "p95_ms": round(p95, 3),
        "p99_ms": round(p99, 3),
        "throughput_mbps": round(mbps, 3),
        "throughput_ops_per_sec": round(ops, 3),
        "errors": errs,
        "runs": len(runs),
    }

# Build doc block — flat table sorted by (target, workload, conc) so re-runs
# on the same jsonl produce identical bytes.
def sort_key(item):
    (target, workload, obj_size, conc), _ = item
    target_order = {"strata": 0, "rgw": 1, "cassandra": 2}.get(target, 3)
    return (workload, target_order, conc)

lines = []
lines.append("<!-- BENCH-AUTO-START -->")
lines.append("")
lines.append("## Latest refresh (auto-generated)")
lines.append("")
lines.append(f"_Last refresh: {refresh_date} via `.github/workflows/bench-rgw.yml`._")
lines.append("")
lines.append(f"- Strata commit: `{commit_sha}`")
lines.append(f"- RGW image: `{rgw_image}`")
lines.append(f"- Source JSONL: `{os.path.basename(jsonl_path)}`")
if runner_info:
    lines.append("")
    lines.append("```text")
    lines.append(runner_info)
    lines.append("```")
lines.append("")
lines.append("| target | workload | object size | concurrency | p50 ms | p95 ms | p99 ms | MB/s | ops/s | errors | runs |")
lines.append("| ------ | -------- | ----------- | ----------- | ------ | ------ | ------ | ---- | ----- | ------ | ---- |")
for key, v in sorted(agg.items(), key=sort_key):
    target, workload, obj_size, conc = key
    lines.append(
        f"| {target} | {workload} | {obj_size or '-'} | {conc} | "
        f"{v['p50_ms']:.3f} | {v['p95_ms']:.3f} | {v['p99_ms']:.3f} | "
        f"{v['throughput_mbps']:.3f} | {v['throughput_ops_per_sec']:.3f} | "
        f"{v['errors']} | {v['runs']} |"
    )
lines.append("")
lines.append("<!-- BENCH-AUTO-END -->")

block = "\n".join(lines) + "\n"
print(block, end="")

# Persist new baseline (sorted keys → byte-stable diff).
baseline_out = {}
for (target, workload, obj_size, conc), v in agg.items():
    bkey = f"{target}|{workload}|{obj_size}|{conc}"
    baseline_out[bkey] = v
with open(baseline_path + ".new", "w") as fh:
    json.dump(
        {"refresh_date": refresh_date, "commit": commit_sha, "rgw_image": rgw_image, "rows": baseline_out},
        fh,
        indent=2,
        sort_keys=True,
    )
    fh.write("\n")

# Regression vs previous baseline (Strata target only — RGW jitter is large
# and noisy on the lima envelope, so the gate watches the Strata side per PRD).
prev = None
if os.path.exists(baseline_path):
    try:
        with open(baseline_path) as fh:
            prev = json.load(fh)
    except (OSError, json.JSONDecodeError):
        prev = None

max_reg_pct = 0.0
max_reg_row = ""
if prev and isinstance(prev.get("rows"), dict):
    for bkey, new_v in baseline_out.items():
        if not bkey.startswith("strata|"):
            continue
        old_v = prev["rows"].get(bkey)
        if not old_v:
            continue
        old_p99 = float(old_v.get("p99_ms", 0))
        new_p99 = float(new_v.get("p99_ms", 0))
        if old_p99 <= 0:
            continue
        delta_pct = (new_p99 - old_p99) / old_p99 * 100.0
        if delta_pct > max_reg_pct:
            max_reg_pct = delta_pct
            max_reg_row = f"{bkey} old={old_p99} new={new_p99}"

with open(baseline_path + ".regression", "w") as fh:
    fh.write(f"{max_reg_pct:.2f}\n")
    if max_reg_row:
        fh.write(max_reg_row + "\n")
PY
)"

regression_pct="$(cat "${BENCH_BASELINE_FILE}.regression" 2>/dev/null | head -1 || echo 0.00)"
regression_detail="$(cat "${BENCH_BASELINE_FILE}.regression" 2>/dev/null | sed -n '2,$p' || true)"

# Splice generated_block into doc between markers. Create markers if absent
# (first-time rollout). Doc edit is atomic via temp file.
tmpdoc="$(mktemp)"
python3 - "$BENCH_DOC_FILE" "$tmpdoc" <<PY "$generated_block"
import sys
doc_path, out_path = sys.argv[1:3]
block = sys.argv[3] if len(sys.argv) > 3 else ""
with open(doc_path) as fh:
    text = fh.read()
start = "<!-- BENCH-AUTO-START -->"
end = "<!-- BENCH-AUTO-END -->"
i = text.find(start)
j = text.find(end)
if i >= 0 and j >= 0 and j > i:
    head = text[:i].rstrip("\n") + "\n\n"
    tail = text[j + len(end):].lstrip("\n")
    new_text = head + block.rstrip("\n") + "\n\n" + tail
elif "## Workload-by-workload" in text:
    # First-time rollout: insert immediately before "## Workload-by-workload".
    anchor = "## Workload-by-workload"
    pos = text.find(anchor)
    head = text[:pos].rstrip("\n") + "\n\n"
    new_text = head + block.rstrip("\n") + "\n\n" + text[pos:]
else:
    # No anchor — append at end.
    new_text = text.rstrip("\n") + "\n\n" + block.rstrip("\n") + "\n"
with open(out_path, "w") as fh:
    fh.write(new_text)
PY

if [[ "$MODE" == "check" ]]; then
  if ! diff -q "$BENCH_DOC_FILE" "$tmpdoc" >/dev/null; then
    echo "FAIL: doc drift detected (run scripts/bench-update-doc.sh to refresh)" >&2
    diff "$BENCH_DOC_FILE" "$tmpdoc" | head -40 >&2 || true
    rm -f "$tmpdoc" "${BENCH_BASELINE_FILE}.new" "${BENCH_BASELINE_FILE}.regression"
    exit 1
  fi
  echo "OK: doc matches latest jsonl ($latest_jsonl)"
  rm -f "$tmpdoc" "${BENCH_BASELINE_FILE}.new" "${BENCH_BASELINE_FILE}.regression"
  exit 0
fi

# Detect changed?
changed=false
if ! diff -q "$BENCH_DOC_FILE" "$tmpdoc" >/dev/null; then
  changed=true
fi
if [[ -f "$BENCH_BASELINE_FILE" ]]; then
  if ! diff -q "$BENCH_BASELINE_FILE" "${BENCH_BASELINE_FILE}.new" >/dev/null; then
    changed=true
  fi
else
  changed=true
fi

mv "$tmpdoc" "$BENCH_DOC_FILE"
mv "${BENCH_BASELINE_FILE}.new" "$BENCH_BASELINE_FILE"

echo "max_regression_pct=$regression_pct"
echo "changed=$changed"
if [[ -n "$regression_detail" ]]; then
  echo "regression_detail=$regression_detail"
fi
if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
  {
    echo "max_regression_pct=$regression_pct"
    echo "changed=$changed"
    [[ -n "$regression_detail" ]] && echo "regression_detail=$regression_detail"
  } >> "$GITHUB_OUTPUT"
fi

rm -f "${BENCH_BASELINE_FILE}.regression"
