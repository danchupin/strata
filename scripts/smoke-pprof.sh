#!/usr/bin/env bash
# pprof endpoint smoke (US-004 of ralph/prod-observability).
#
# Boots strata with STRATA_PPROF_ENABLED=true on a dedicated listener,
# captures a heap profile under admin auth, and decodes the bytes via the
# Go-native helper (internal/pprofutil.Parse — google/pprof/profile.Parse).
# No runtime `go tool pprof` dependency, no BusyBox-fragile shell tooling.
#
# Memory-backed; no Docker / Cassandra / TiKV needed. Exits non-zero on
# the first failed check.

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

echo "== build strata"
make build >/dev/null
[[ -x "${BIN}" ]] || { echo "FAIL: build_produces_strata"; exit 1; }

MAIN_PORT=19000
ADMIN_PORT=19001
PPROF_PORT=19002

AK="AKADMIN"
SK="SKADMIN"

echo "== start strata main=:${MAIN_PORT} admin=127.0.0.1:${ADMIN_PORT} pprof=127.0.0.1:${PPROF_PORT}"
STRATA_LISTEN=":${MAIN_PORT}" \
STRATA_ADMIN_LISTEN="127.0.0.1:${ADMIN_PORT}" \
STRATA_PPROF_ENABLED=true \
STRATA_PPROF_LISTEN="127.0.0.1:${PPROF_PORT}" \
STRATA_PPROF_BLOCK_RATE=1000 \
STRATA_PPROF_MUTEX_RATE=10 \
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

echo "== probe /debug/pprof/heap on dedicated pprof listener (anon, expect 401)"
status="$(curl -s -o /dev/null -w "%{http_code}" "http://127.0.0.1:${PPROF_PORT}/debug/pprof/heap")"
[[ "${status}" == "401" ]] || { echo "FAIL: anon pprof status=${status} want 401"; exit 1; }

echo "== probe /debug/pprof/heap on admin listener (should be 404 — pprof has dedicated listener)"
status="$(curl -s -o /dev/null -w "%{http_code}" "http://127.0.0.1:${ADMIN_PORT}/debug/pprof/heap")"
[[ "${status}" == "404" ]] || { echo "FAIL: admin pprof leak status=${status} want 404"; exit 1; }

echo "== capture heap profile via SigV4-signed curl"
# aws-cli would be ideal but isn't a smoke prerequisite; use a tiny Go
# one-shot to do SigV4 the same way internal/serverapp/pprof_test.go does.
HEAP_OUT="${TMP}/heap.pprof"
go run ./scripts/pprof-fetch \
    --url "http://127.0.0.1:${PPROF_PORT}/debug/pprof/heap" \
    --access-key "${AK}" \
    --secret "${SK}" \
    --region "strata-local" \
    --out "${HEAP_OUT}"

[[ -s "${HEAP_OUT}" ]] || { echo "FAIL: heap profile empty"; exit 1; }

echo "== decode heap profile via Go-native helper"
STRATA_PPROF_SMOKE_PROFILE="${HEAP_OUT}" \
    go test -run TestPprofDecode -count=1 ./internal/serverapp/... >"${TMP}/decode.log" 2>&1 || {
    cat "${TMP}/decode.log"
    echo "FAIL: pprof decode failed — captured bytes are not a valid profile"
    exit 1
}

echo "== probe /debug/pprof/profile (CPU) — short capture to confirm endpoint shape"
PROF_OUT="${TMP}/cpu.pprof"
go run ./scripts/pprof-fetch \
    --url "http://127.0.0.1:${PPROF_PORT}/debug/pprof/profile?seconds=1" \
    --access-key "${AK}" \
    --secret "${SK}" \
    --region "strata-local" \
    --out "${PROF_OUT}"
[[ -s "${PROF_OUT}" ]] || { echo "FAIL: cpu profile empty"; exit 1; }

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

echo "PASS: pprof smoke (heap + cpu captured + decoded; all 3 listeners drained on TERM)"
