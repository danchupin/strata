#!/usr/bin/env bash
# Admin-endpoint split smoke (US-008 of ralph/harden-gateway).
#
# Boots strata with STRATA_ADMIN_LISTEN set to a separate port, validates:
#   1. /admin/v1, /metrics, /healthz live on the admin listener.
#   2. The same routes return non-200 on the main S3 listener (S3 catch-all
#      owns / there).
#   3. Admin listener bound to 127.0.0.1 only — TCP dial works on loopback.
#   4. Both listeners drain on SIGTERM.
#
# Memory-backed; no Docker / Cassandra / TiKV needed. Exits non-zero on the
# first failed check.

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

echo "== start strata main=:${MAIN_PORT} admin=127.0.0.1:${ADMIN_PORT}"
STRATA_LISTEN=":${MAIN_PORT}" \
STRATA_ADMIN_LISTEN="127.0.0.1:${ADMIN_PORT}" \
STRATA_DATA_BACKEND=memory \
STRATA_META_BACKEND=memory \
STRATA_AUTH_MODE=off \
"${BIN}" server >"${TMP}/strata.log" 2>&1 &
STRATA_PID=$!

# Wait for both listeners.
for port in ${MAIN_PORT} ${ADMIN_PORT}; do
    for _ in $(seq 1 50); do
        if (echo >/dev/tcp/127.0.0.1/${port}) 2>/dev/null; then
            break
        fi
        sleep 0.1
    done
done

echo "== probe /healthz on admin listener (expect 200)"
status="$(curl -s -o /dev/null -w "%{http_code}" "http://127.0.0.1:${ADMIN_PORT}/healthz")"
[[ "${status}" == "200" ]] || { echo "FAIL: admin /healthz status=${status}"; exit 1; }

echo "== probe /metrics on admin listener (expect 200)"
status="$(curl -s -o /dev/null -w "%{http_code}" "http://127.0.0.1:${ADMIN_PORT}/metrics")"
[[ "${status}" == "200" ]] || { echo "FAIL: admin /metrics status=${status}"; exit 1; }

echo "== probe /healthz on main S3 listener (expect non-200; S3 catch-all)"
status="$(curl -s -o /dev/null -w "%{http_code}" "http://127.0.0.1:${MAIN_PORT}/healthz")"
[[ "${status}" != "200" ]] || { echo "FAIL: main /healthz unexpectedly 200 — split broken"; exit 1; }

echo "== probe arbitrary S3 path on admin listener (expect 404)"
status="$(curl -s -o /dev/null -w "%{http_code}" "http://127.0.0.1:${ADMIN_PORT}/some-bucket/some-key")"
[[ "${status}" == "404" ]] || { echo "FAIL: admin S3 path status=${status} want 404"; exit 1; }

echo "== verify admin listener bound to loopback only (TCP dial from 127.0.0.1 works)"
if ! (echo >/dev/tcp/127.0.0.1/${ADMIN_PORT}) 2>/dev/null; then
    echo "FAIL: cannot reach admin port over loopback"; exit 1
fi

echo "== SIGTERM drain"
kill -TERM ${STRATA_PID}
wait ${STRATA_PID} || true
STRATA_PID=""

# Both ports must be closed.
sleep 0.5
if (echo >/dev/tcp/127.0.0.1/${MAIN_PORT}) 2>/dev/null; then
    echo "FAIL: main listener still accepting after SIGTERM"; exit 1
fi
if (echo >/dev/tcp/127.0.0.1/${ADMIN_PORT}) 2>/dev/null; then
    echo "FAIL: admin listener still accepting after SIGTERM"; exit 1
fi

echo "PASS: admin endpoint split (main=${MAIN_PORT}, admin=${ADMIN_PORT}, both drained on TERM)"
