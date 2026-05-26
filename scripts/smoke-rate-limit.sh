#!/usr/bin/env bash
# Ingress rate limiter smoke (US-009 of ralph/harden-gateway).
#
# Boots strata with STRATA_RATE_LIMIT_PER_IP=5 + burst=2, fires ~30 GETs
# back-to-back, and asserts:
#   1. Some requests return HTTP 429 (rate limiter fired).
#   2. The 429 response body carries <Code>SlowDown</Code> + Retry-After: 1.
#   3. /healthz still returns 200 (admin/health bypass the limiter).
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

PORT=19500

echo "== start strata listen=:${PORT} per_ip=5 burst=2"
STRATA_LISTEN=":${PORT}" \
STRATA_DATA_BACKEND=memory \
STRATA_META_BACKEND=memory \
STRATA_AUTH_MODE=off \
STRATA_RATE_LIMIT_PER_IP=5 \
STRATA_RATE_LIMIT_BURST=2 \
STRATA_RATE_LIMIT_CACHE_SIZE=1000 \
"${BIN}" server >"${TMP}/strata.log" 2>&1 &
STRATA_PID=$!

for _ in $(seq 1 50); do
    if (echo >/dev/tcp/127.0.0.1/${PORT}) 2>/dev/null; then
        break
    fi
    sleep 0.1
done
if ! (echo >/dev/tcp/127.0.0.1/${PORT}) 2>/dev/null; then
    cat "${TMP}/strata.log"
    echo "FAIL: listener_ready"
    exit 1
fi

echo "== probe /healthz (bypasses rate limiter)"
for _ in $(seq 1 5); do
    code=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:${PORT}/healthz")
    if [[ "${code}" != "200" ]]; then
        echo "FAIL: healthz_bypass_limiter code=${code}"
        exit 1
    fi
done
echo "PASS: healthz_bypass_limiter"

echo "== flood / with 30 GETs"
PASS=0
REFUSED=0
LAST_BODY="${TMP}/last-429.xml"
LAST_HEADERS="${TMP}/last-429.headers"
for _ in $(seq 1 30); do
    body="${TMP}/body"
    headers="${TMP}/headers"
    code=$(curl -s -o "${body}" -D "${headers}" -w '%{http_code}' "http://127.0.0.1:${PORT}/somebucket/")
    if [[ "${code}" == "429" ]]; then
        REFUSED=$((REFUSED + 1))
        cp "${body}" "${LAST_BODY}"
        cp "${headers}" "${LAST_HEADERS}"
    else
        PASS=$((PASS + 1))
    fi
done
echo "  passed=${PASS} refused=${REFUSED}"

if (( REFUSED == 0 )); then
    echo "FAIL: rate_limit_never_fired (per_ip=5 + burst=2 should refuse some)"
    exit 1
fi
if ! grep -q '<Code>SlowDown</Code>' "${LAST_BODY}"; then
    echo "FAIL: 429_body_missing_SlowDown"
    cat "${LAST_BODY}"
    exit 1
fi
if ! grep -qi '^Retry-After: *1' "${LAST_HEADERS}"; then
    echo "FAIL: 429_missing_retry_after"
    cat "${LAST_HEADERS}"
    exit 1
fi
echo "PASS: rate_limit_refuses_with_slowdown"

echo "== sleep 1.5s, probe /somebucket/ again (bucket refilled)"
sleep 1.5
code=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:${PORT}/somebucket/")
if [[ "${code}" == "429" ]]; then
    echo "FAIL: post_refill_still_refused (per_ip=5 should refill after 1s)"
    exit 1
fi
echo "PASS: rate_limit_refills"

echo "== shutdown"
kill_strata
echo "PASS: smoke-rate-limit"
