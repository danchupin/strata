#!/usr/bin/env bash
# US-012 - smoke + s3-backend invariant assertions.
#
# Wraps scripts/smoke.sh and adds three assertions tied to the s3-backend
# data plane:
#   1. After smoke.sh tears down its objects, the backend bucket is empty
#      (proves enqueueOrphan synchronously cleans up backend objects).
#   2. PUT N small + 1 multipart yields exactly N+1 backend objects (1:1
#      invariant — never one-strata-object-as-N-backend-objects).
#   3. After deleting only the small objects, exactly 1 backend object
#      remains (multipart object stays as a single backend object, not
#      its constituent parts).
#   4. (Optional, gated by SKIP_DOWN=0) `make down` removes the
#      strata-minio-data docker volume.
#
# Env knobs:
#   STRATA_S3_BACKEND_BUCKET (default strata-backend) — backend bucket name
#   MINIO_ROOT_USER          (default stratauser)
#   MINIO_ROOT_PASSWORD      (default stratapass)
#   COMPOSE_NETWORK          (default docker_default) — docker network mc joins
#   COMPOSE_VOLUME           (default docker_strata-minio-data) — volume probed by the down test
#   SKIP_DOWN                (default 1 for local dev; CI sets 0)

set -euo pipefail

BASE="${1:-http://127.0.0.1:9999}"
BUCKET="${STRATA_S3_BACKEND_BUCKET:-strata-backend}"
MINIO_USER="${MINIO_ROOT_USER:-stratauser}"
MINIO_PASS="${MINIO_ROOT_PASSWORD:-stratapass}"
NETWORK="${COMPOSE_NETWORK:-docker_default}"
VOLUME="${COMPOSE_VOLUME:-docker_strata-minio-data}"
SKIP_DOWN="${SKIP_DOWN:-1}"

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

mc_run() {
  docker run --rm --network "$NETWORK" \
    -e "MC_HOST_strata=http://${MINIO_USER}:${MINIO_PASS}@minio:9000" \
    --entrypoint mc minio/mc:latest "$@"
}

backend_obj_count() {
  mc_run find "strata/$BUCKET" --type f 2>/dev/null | wc -l | tr -d ' '
}

dump_backend() {
  echo "  backend listing (strata/$BUCKET):"
  mc_run find "strata/$BUCKET" --type f 2>/dev/null | sed 's/^/    /' || true
}

assert_count() {
  local want="$1" label="$2"
  local got
  got="$(backend_obj_count)"
  if [ "$got" != "$want" ]; then
    echo "  FAIL: $label: expected $want backend objects, got $got"
    dump_backend
    exit 1
  fi
  echo "  ok: $label: $got backend objects"
}

echo "== pre-check: backend bucket empty before smoke"
assert_count 0 "fresh state"

echo "== running scripts/smoke.sh against $BASE"
bash "$SCRIPT_DIR/smoke.sh" "$BASE"

echo "== post-smoke: backend bucket empty (orphan cleanup ran)"
assert_count 0 "post-smoke cleanup"

echo "== 1:1 invariant test (5 small + 1 multipart -> 6 backend objects)"
TBUCKET="onetoone-$(date +%s)"
curl -sf -o /dev/null -X PUT "$BASE/$TBUCKET"

for i in 1 2 3 4 5; do
  curl -sf -o /dev/null -X PUT --data-binary "object-$i" \
    "$BASE/$TBUCKET/obj-$i"
done

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
dd if=/dev/urandom of="$TMP/mp.bin" bs=1M count=18 2>/dev/null
split -b 6M "$TMP/mp.bin" "$TMP/p."
PARTS=("$TMP"/p.*)

INIT_XML="$(curl -sf -X POST "$BASE/$TBUCKET/multi?uploads")"
UPID="$(printf '%s' "$INIT_XML" | sed -n 's:.*<UploadId>\([^<]*\)</UploadId>.*:\1:p')"
[ -n "$UPID" ] || { echo "  FAIL: no uploadId returned: $INIT_XML"; exit 1; }

PXML=""
N=0
for f in "${PARTS[@]}"; do
  N=$((N+1))
  ETAG="$(curl -sf -o /dev/null -w '%header{etag}' -X PUT --data-binary @"$f" \
    "$BASE/$TBUCKET/multi?uploadId=$UPID&partNumber=$N" | tr -d '\r"')"
  [ -n "$ETAG" ] || { echo "  FAIL: part $N: no etag"; exit 1; }
  PXML+="<Part><PartNumber>$N</PartNumber><ETag>\"$ETAG\"</ETag></Part>"
done

curl -sf -X POST --data "<CompleteMultipartUpload>$PXML</CompleteMultipartUpload>" \
  "$BASE/$TBUCKET/multi?uploadId=$UPID" >/dev/null

assert_count 6 "5 small PUTs + 1 multipart Complete"

for i in 1 2 3 4 5; do
  curl -sf -o /dev/null -X DELETE "$BASE/$TBUCKET/obj-$i"
done

assert_count 1 "after deleting 5 small (multipart stays as 1 backend object, not $N parts)"

curl -sf -o /dev/null -X DELETE "$BASE/$TBUCKET/multi"
curl -sf -o /dev/null -X DELETE "$BASE/$TBUCKET"

assert_count 0 "post-1:1 cleanup"

echo "== smoke-s3-backend OK (1:1 invariant + multipart 1:1 verified)"

if [ "$SKIP_DOWN" != "0" ]; then
  echo "== SKIP_DOWN=$SKIP_DOWN, skipping make down volume cleanup test"
  echo "== run with SKIP_DOWN=0 to verify make down clears $VOLUME"
  exit 0
fi

echo "== verifying make down removes $VOLUME"
( cd "$REPO_ROOT" && make down ) >/dev/null
if docker volume inspect "$VOLUME" >/dev/null 2>&1; then
  echo "  FAIL: volume $VOLUME still present after make down"
  exit 1
fi
echo "  ok: $VOLUME removed by make down"
echo "== smoke-s3-backend full pass (with down cleanup verification)"
