#!/usr/bin/env bash
# scripts/smoke-image-verification.sh — cosign sign/verify roundtrip smoke
# (Cycle C supply-chain-security, US-008).
#
# Proves the cosign signing contract WITHOUT requiring a GHCR push or the
# GitHub Actions OIDC identity: it builds a tiny throwaway image, pushes it
# to an EPHEMERAL local registry, signs it BY DIGEST with a locally
# generated key pair (`cosign sign --key cosign.key`), and verifies the
# signature with the matching public key. A negative pass confirms that a
# WRONG public key fails verification — so the smoke catches a no-op
# verify that would silently pass anything.
#
# This mirrors the keyless OIDC flow shipped in
# `.github/workflows/release-image.yml :: cosign-sign` (US-008): same
# sign-by-digest, same registry-resident signature artifact. The CI path
# is keyless (Fulcio + Rekor); this dev smoke is key-based for offline
# reproducibility. Both sign the image manifest digest, never a tag.
#
# Degrades to WARN + exit 0 when cosign or a working docker daemon is
# missing (matches the helm-lint / promtool-check / check-govulncheck.sh
# degradation pattern) so `make smoke-supply-chain` is not gated on the
# toolchain being installed.

set -euo pipefail

cd "$(dirname "$0")/.."
# shellcheck source=scripts/lib/severity.sh
source "$(dirname "$0")/lib/severity.sh"

warn_skip() {
    echo "$(severity_yellow)WARN$(severity_reset) image-verification smoke SKIPPED: $1"
    exit 0
}

command -v cosign >/dev/null 2>&1 || warn_skip "cosign not on PATH"
command -v docker >/dev/null 2>&1 || warn_skip "docker not on PATH"
docker info >/dev/null 2>&1 || warn_skip "docker daemon not reachable"

TMP="$(mktemp -d)"
REG_CONTAINER="strata-cosign-smoke-registry-$$"
# Ephemeral host port for the throwaway registry. Fixed-but-uncommon port;
# the registry container is torn down on EXIT regardless of outcome.
REG_PORT=5957
REG="localhost:${REG_PORT}"
IMAGE="${REG}/strata-cosign-smoke:dev"

cleanup() {
    docker rm -f "${REG_CONTAINER}" >/dev/null 2>&1 || true
    rm -rf "${TMP}"
}
trap cleanup EXIT

# cosign refuses an empty password unless COSIGN_PASSWORD is explicitly set.
export COSIGN_PASSWORD=""

# A cold cosign cache initialises its Sigstore TUF trust root on first use,
# which reaches the network and can hang on an air-gapped / flaky host. Wrap
# every cosign invocation in a timeout so the smoke degrades to WARN instead
# of hanging forever; absent a `timeout`/`gtimeout` binary, run bare.
COSIGN_TIMEOUT=90
if command -v timeout >/dev/null 2>&1; then
    _timeout() { timeout "$@"; }
elif command -v gtimeout >/dev/null 2>&1; then
    _timeout() { gtimeout "$@"; }
else
    _timeout() { shift; "$@"; } # drop the duration arg, run bare
fi

# Local HTTP registry needs the insecure flags. --use-signing-config=false +
# --tlog-upload=false keep signing fully offline (no Rekor / Fulcio) for
# dev-mode reproducibility; --insecure-ignore-tlog=true mirrors that on verify.
HTTP_FLAGS=(--allow-http-registry --allow-insecure-registry)

echo "== start ephemeral local registry ${REG}"
docker run -d --rm -p "${REG_PORT}:5000" --name "${REG_CONTAINER}" registry:2 >/dev/null
for _ in $(seq 1 50); do
    if (echo >/dev/tcp/127.0.0.1/${REG_PORT}) 2>/dev/null; then
        break
    fi
    sleep 0.1
done
(echo >/dev/tcp/127.0.0.1/${REG_PORT}) 2>/dev/null || { echo "FAIL: local registry never came up"; exit 1; }

echo "== build + push a tiny locally-built image"
# A throwaway image (not strata:ceph) keeps the smoke fast — the contract
# under test is the cosign sign→verify roundtrip on a manifest digest, not
# the strata build (which the release-image.yml docker-build job + US-002
# trivy job already exercise).
cat >"${TMP}/Dockerfile" <<'EOF'
FROM alpine:3.20
LABEL org.opencontainers.image.title="strata-cosign-smoke"
RUN echo "strata cosign smoke fixture" > /etc/strata-smoke
EOF
docker build -q -t "${IMAGE}" "${TMP}" >/dev/null
docker push -q "${IMAGE}" >/dev/null 2>&1 || docker push "${IMAGE}" >/dev/null

# Resolve the pushed manifest digest — sign BY DIGEST, never by tag.
DIGEST="$(docker inspect --format '{{index .RepoDigests 0}}' "${IMAGE}" 2>/dev/null | cut -d'@' -f2 || true)"
[[ -n "${DIGEST}" ]] || { echo "FAIL: could not resolve pushed image digest"; exit 1; }
IMAGE_REF="${REG}/strata-cosign-smoke@${DIGEST}"
echo "   digest=${DIGEST}"

echo "== generate cosign key pair (offline dev mode)"
( cd "${TMP}" && cosign generate-key-pair >/dev/null )
[[ -s "${TMP}/cosign.key" && -s "${TMP}/cosign.pub" ]] || { echo "FAIL: cosign key pair not generated"; exit 1; }

# A SECOND, unrelated key pair for the negative-verify pass.
mkdir -p "${TMP}/wrong"
( cd "${TMP}/wrong" && cosign generate-key-pair >/dev/null )

echo "== cosign sign --key (by digest)"
sign_rc=0
_timeout "${COSIGN_TIMEOUT}" cosign sign --yes --use-signing-config=false --tlog-upload=false \
    "${HTTP_FLAGS[@]}" --key "${TMP}/cosign.key" "${IMAGE_REF}" >/dev/null 2>&1 || sign_rc=$?
if [[ "${sign_rc}" == "124" ]]; then
    warn_skip "cosign sign timed out after ${COSIGN_TIMEOUT}s (cold TUF init / no network?)"
fi
[[ "${sign_rc}" == "0" ]] || { echo "FAIL: cosign sign failed (rc=${sign_rc})"; exit 1; }

echo "== cosign verify with correct key (expect PASS)"
_timeout "${COSIGN_TIMEOUT}" cosign verify --insecure-ignore-tlog=true \
    "${HTTP_FLAGS[@]}" --key "${TMP}/cosign.pub" "${IMAGE_REF}" >/dev/null 2>&1 \
    || { echo "FAIL: cosign verify with correct key did NOT pass"; exit 1; }

echo "== cosign verify with WRONG key (expect FAIL — guards against no-op verify)"
if _timeout "${COSIGN_TIMEOUT}" cosign verify --insecure-ignore-tlog=true \
    "${HTTP_FLAGS[@]}" --key "${TMP}/wrong/cosign.pub" "${IMAGE_REF}" >/dev/null 2>&1; then
    echo "FAIL: cosign verify with WRONG key unexpectedly PASSED — verify is a no-op"
    exit 1
fi

echo "$(severity_green)PASS$(severity_reset) image-verification smoke: sign→verify roundtrip OK; wrong-key verify correctly rejected"
