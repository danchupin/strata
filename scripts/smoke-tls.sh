#!/usr/bin/env bash
# Built-in TLS listener smoke (US-002 of ralph/harden-gateway).
#
# Runs the strata binary with a self-signed cert + STRATA_TLS_* envs,
# validates:
#   1. HTTPS handshake completes (openssl s_client)
#   2. HTTP/2 ALPN negotiation succeeds (curl --http2 with explicit cacert)
#   3. /healthz returns 200 over HTTPS
#   4. Negotiated TLS version ≥ 1.2
#
# Memory-backed; no Docker, no Cassandra/TiKV needed. Exits non-zero on
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

echo "== generate self-signed cert"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 \
    -keyout "${TMP}/key.pem" -out "${TMP}/cert.pem" \
    -days 1 -nodes -subj "/CN=127.0.0.1" \
    -addext "subjectAltName=IP:127.0.0.1" >/dev/null 2>&1

echo "== start strata on :19443 with TLS"
PORT=19443
STRATA_LISTEN=":${PORT}" \
STRATA_DATA_BACKEND=memory \
STRATA_META_BACKEND=memory \
STRATA_AUTH_MODE=off \
STRATA_TLS_CERT_FILE="${TMP}/cert.pem" \
STRATA_TLS_KEY_FILE="${TMP}/key.pem" \
STRATA_TLS_MIN_VERSION=TLS1.2 \
STRATA_TLS_CIPHER_PROFILE=mozilla-modern \
"${BIN}" server >"${TMP}/strata.log" 2>&1 &
STRATA_PID=$!

# Wait for listener.
for _ in $(seq 1 50); do
    if (echo >/dev/tcp/127.0.0.1/${PORT}) 2>/dev/null; then
        break
    fi
    sleep 0.1
done

echo "== probe HTTPS handshake (curl verbose)"
hs_out="$(curl -sv --cacert "${TMP}/cert.pem" "https://127.0.0.1:${PORT}/healthz" 2>&1 >/dev/null)"
proto="$(grep -oE 'SSL connection using TLSv1\.[23]' <<<"${hs_out}" | head -n1 | awk '{print $NF}')"
if [[ -z "${proto}" ]]; then
    echo "FAIL: negotiated_tls_version_>=_1.2"
    echo "${hs_out}" | tail -30
    exit 1
fi
echo "  negotiated ${proto}"
if ! grep -qE 'SSL certificate verify ok' <<<"${hs_out}"; then
    echo "FAIL: handshake_verify"
    echo "${hs_out}" | tail -30
    exit 1
fi

echo "== probe /healthz over HTTPS"
status="$(curl -s -o /dev/null -w "%{http_code}" --cacert "${TMP}/cert.pem" "https://127.0.0.1:${PORT}/healthz")"
if [[ "${status}" != "200" ]]; then
    echo "FAIL: healthz_200 (got ${status})"
    exit 1
fi

echo "== probe HTTP/2 ALPN negotiation"
h2_out="$(curl -sI --http2 --cacert "${TMP}/cert.pem" "https://127.0.0.1:${PORT}/healthz")"
if ! grep -qiE '^HTTP/2' <<<"${h2_out}"; then
    echo "FAIL: http2_alpn"
    echo "${h2_out}"
    exit 1
fi

echo "== probe cert content (openssl x509)"
subj="$(openssl x509 -in "${TMP}/cert.pem" -noout -subject)"
if ! grep -q "CN" <<<"${subj}"; then
    echo "FAIL: cert_subject_cn"
    exit 1
fi

echo "PASS: built-in TLS listener (proto=${proto}, healthz=200, h2=ok, subj=${subj})"
