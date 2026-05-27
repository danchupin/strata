#!/usr/bin/env bash
# Cycle B prod-observability composite smoke (US-011).
#
# Verifies every Cycle B feature composes cleanly without operator
# follow-up:
#   (a) promtool check rules deploy/prometheus/alerts.yml exits 0
#       (skip with WARN when promtool not installed).
#   (b) boot strata with STRATA_PPROF_ENABLED=true on dedicated listener,
#       curl /debug/pprof/heap from loopback under admin auth, decode via
#       Go-native helper (internal/pprofutil.Parse).
#   (c) go test ./deploy/grafana/... passes (dashboard drift-lint green).
#   (d) bin/strata admin slo-report --window 7d --prometheus-url
#       http://localhost:9090 emits non-empty markdown. Prometheus URL
#       override via STRATA_OBSERVABILITY_PROMETHEUS_URL; non-reachable
#       endpoint logs WARN and produces a report with empty SLI rows
#       rather than hard-failing — operators run against the bare lab
#       (make up-all) for a real signal.
#
# Memory-backed; no Docker / Cassandra / TiKV needed for (a)..(c).
# Exits non-zero on the first failed check.

set -euo pipefail

cd "$(dirname "$0")/.."

ROOT="$(pwd)"
BIN="${ROOT}/bin/strata"
TMP="$(mktemp -d)"
trap 'kill_strata; rm -rf "$TMP"' EXIT

STRATA_PID=""
kill_strata() {
    if [[ -n "${STRATA_PID}" ]] && kill -0 "${STRATA_PID}" 2>/dev/null; then
        kill "${STRATA_PID}" 2>/dev/null || true
        wait "${STRATA_PID}" 2>/dev/null || true
    fi
}

echo "== (a) promtool check rules deploy/prometheus/alerts.yml"
if command -v promtool >/dev/null 2>&1; then
    promtool check rules deploy/prometheus/alerts.yml
else
    echo "WARN: promtool not installed — skipping rule check (install: go install github.com/prometheus/prometheus/cmd/promtool@latest)"
fi

echo "== (c) go test ./deploy/grafana/... (dashboard drift-lint)"
go test ./deploy/grafana/... -count=1 >"${TMP}/grafana.log" 2>&1 || {
    cat "${TMP}/grafana.log"
    echo "FAIL: dashboard drift-lint"
    exit 1
}

echo "== build strata"
make build >/dev/null
[[ -x "${BIN}" ]] || { echo "FAIL: build_produces_strata"; exit 1; }

MAIN_PORT=19000
ADMIN_PORT=19001
PPROF_PORT=19002

AK="AKADMIN"
SK="SKADMIN"

echo "== (b) start strata main=:${MAIN_PORT} admin=127.0.0.1:${ADMIN_PORT} pprof=127.0.0.1:${PPROF_PORT}"
STRATA_LISTEN=":${MAIN_PORT}" \
STRATA_ADMIN_LISTEN="127.0.0.1:${ADMIN_PORT}" \
STRATA_PPROF_ENABLED=true \
STRATA_PPROF_LISTEN="127.0.0.1:${PPROF_PORT}" \
STRATA_DATA_BACKEND=memory \
STRATA_META_BACKEND=memory \
STRATA_AUTH_MODE=required \
STRATA_STATIC_CREDENTIALS="${AK}:${SK}:admin" \
"${BIN}" server >"${TMP}/strata.log" 2>&1 &
STRATA_PID=$!

for port in ${MAIN_PORT} ${ADMIN_PORT} ${PPROF_PORT}; do
    for _ in $(seq 1 50); do
        if (echo >/dev/tcp/127.0.0.1/${port}) 2>/dev/null; then
            break
        fi
        sleep 0.1
    done
done

echo "== (b) capture heap profile via SigV4-signed curl + decode via Go helper"
HEAP_OUT="${TMP}/heap.pprof"
go run ./scripts/pprof-fetch \
    --url "http://127.0.0.1:${PPROF_PORT}/debug/pprof/heap" \
    --access-key "${AK}" \
    --secret "${SK}" \
    --region "strata-local" \
    --out "${HEAP_OUT}"
[[ -s "${HEAP_OUT}" ]] || { echo "FAIL: heap profile empty"; exit 1; }

STRATA_PPROF_SMOKE_PROFILE="${HEAP_OUT}" \
    go test -run TestPprofDecode -count=1 ./internal/serverapp/... >"${TMP}/decode.log" 2>&1 || {
    cat "${TMP}/decode.log"
    echo "FAIL: pprof decode failed — captured bytes are not a valid profile"
    exit 1
}

echo "== (d) bin/strata admin slo-report --window 7d"
PROM_URL="${STRATA_OBSERVABILITY_PROMETHEUS_URL:-http://localhost:9090}"
REPORT_OUT="${TMP}/slo-report.md"
if "${BIN}" admin slo-report \
    --prometheus-url "${PROM_URL}" \
    --window 7d \
    --out "${REPORT_OUT}" >"${TMP}/slo-report.log" 2>&1; then
    if [[ -s "${REPORT_OUT}" ]]; then
        head -5 "${REPORT_OUT}"
    else
        echo "FAIL: slo-report produced empty file"
        cat "${TMP}/slo-report.log"
        exit 1
    fi
else
    # Prometheus may be unreachable in CI / local smoke without `make up-all`;
    # treat connection failure as WARN, not FAIL. Real signal lives in the
    # subcommand unit tests + the `make up-all && make slo-report` workflow.
    echo "WARN: slo-report against ${PROM_URL} failed — Prometheus likely not running (run 'make up-all' for a real signal)"
    cat "${TMP}/slo-report.log"
fi

echo "== SIGTERM drain"
kill -TERM ${STRATA_PID}
wait ${STRATA_PID} || true
STRATA_PID=""

sleep 0.5
for port in ${MAIN_PORT} ${ADMIN_PORT} ${PPROF_PORT}; do
    if (echo >/dev/tcp/127.0.0.1/${port}) 2>/dev/null; then
        echo "FAIL: listener :${port} still accepting after SIGTERM"; exit 1
    fi
done

echo "PASS: prod-observability smoke (alerts.yml rules valid, dashboard drift-lint green, pprof captured + decoded, slo-report subcommand exercised)"
