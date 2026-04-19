#!/usr/bin/env bash
set -euo pipefail

BASE="${1:-http://127.0.0.1:9999}"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

BUCKET="smoke-$(date +%s)"

md5of() {
  if command -v md5 >/dev/null 2>&1; then md5 -q "$1"; else md5sum "$1" | awk '{print $1}'; fi
}

echo "== PUT bucket $BUCKET"
curl -sf -o /dev/null -w "  %{http_code}\n" -X PUT "$BASE/$BUCKET"

echo "== PUT small object"
curl -sf -o /dev/null -w "  %{http_code} etag=%header{etag}\n" \
  -X PUT -H "Content-Type: text/plain" --data-binary "hello strata" \
  "$BASE/$BUCKET/greeting.txt"

echo "== PUT nested objects for prefix/delimiter checks"
curl -sf -o /dev/null -X PUT --data-binary "a" "$BASE/$BUCKET/logs/2026/04/a.log"
curl -sf -o /dev/null -X PUT --data-binary "b" "$BASE/$BUCKET/logs/2026/04/b.log"
curl -sf -o /dev/null -X PUT --data-binary "c" "$BASE/$BUCKET/logs/2026/05/c.log"
curl -sf -o /dev/null -X PUT --data-binary "d" "$BASE/$BUCKET/images/d.jpg"

echo "== PUT 10MB blob (single-shot)"
dd if=/dev/urandom of="$TMP/big.bin" bs=1M count=10 2>/dev/null
ORIG_MD5="$(md5of "$TMP/big.bin")"
curl -sf -o /dev/null -w "  %{http_code}\n" -X PUT --data-binary @"$TMP/big.bin" "$BASE/$BUCKET/big.bin"

echo "== HEAD small"
curl -sf -D- -o /dev/null "$BASE/$BUCKET/greeting.txt" | head -n 6

echo "== GET small"
[ "$(curl -sf "$BASE/$BUCKET/greeting.txt")" = "hello strata" ] && echo "  ok" || { echo "  MISMATCH"; exit 1; }

echo "== GET big and compare md5"
curl -sf "$BASE/$BUCKET/big.bin" -o "$TMP/big-got.bin"
GOT_MD5="$(md5of "$TMP/big-got.bin")"
if [ "$ORIG_MD5" = "$GOT_MD5" ]; then echo "  ok ($GOT_MD5)"; else echo "  MISMATCH $ORIG_MD5 vs $GOT_MD5"; exit 1; fi

echo "== LIST root, default page"
curl -sf "$BASE/$BUCKET?list-type=2" | head -c 500; echo

echo "== LIST with prefix=logs/ delimiter=/ (expect CommonPrefixes)"
curl -sf "$BASE/$BUCKET?list-type=2&prefix=logs/&delimiter=/" | head -c 500; echo

echo "== LIST with prefix=logs/2026/04/"
curl -sf "$BASE/$BUCKET?list-type=2&prefix=logs/2026/04/" | head -c 500; echo

echo "== MULTIPART upload (3 parts x 6 MiB = 18 MiB)"
dd if=/dev/urandom of="$TMP/mp.bin" bs=1M count=18 2>/dev/null
MP_MD5="$(md5of "$TMP/mp.bin")"
split -b 6M "$TMP/mp.bin" "$TMP/mp.part."
PARTS=( "$TMP"/mp.part.* )

INIT_XML="$(curl -sf -X POST "$BASE/$BUCKET/mp-object?uploads")"
UPLOAD_ID="$(printf '%s' "$INIT_XML" | sed -n 's:.*<UploadId>\([^<]*\)</UploadId>.*:\1:p')"
[ -n "$UPLOAD_ID" ] || { echo "  failed to initiate: $INIT_XML"; exit 1; }
echo "  uploadId=$UPLOAD_ID"

PART_XML=""
N=0
for f in "${PARTS[@]}"; do
  N=$((N+1))
  ETAG="$(curl -sf -o /dev/null -w '%header{etag}' -X PUT --data-binary @"$f" \
    "$BASE/$BUCKET/mp-object?uploadId=$UPLOAD_ID&partNumber=$N" | tr -d '\r"')"
  [ -n "$ETAG" ] || { echo "  part $N: no etag"; exit 1; }
  echo "  part $N uploaded etag=$ETAG size=$(wc -c < "$f")"
  PART_XML+="<Part><PartNumber>$N</PartNumber><ETag>\"$ETAG\"</ETag></Part>"
done

echo "== LIST parts"
curl -sf "$BASE/$BUCKET/mp-object?uploadId=$UPLOAD_ID" | head -c 600; echo

echo "== LIST multipart uploads"
curl -sf "$BASE/$BUCKET?uploads" | head -c 300; echo

echo "== COMPLETE multipart"
COMPLETE_BODY="<CompleteMultipartUpload>$PART_XML</CompleteMultipartUpload>"
COMPLETE_RESP="$(curl -sf -X POST --data "$COMPLETE_BODY" "$BASE/$BUCKET/mp-object?uploadId=$UPLOAD_ID")"
echo "  $COMPLETE_RESP"

echo "== GET multipart object, verify md5"
curl -sf "$BASE/$BUCKET/mp-object" -o "$TMP/mp-got.bin"
GOT_MP_MD5="$(md5of "$TMP/mp-got.bin")"
if [ "$MP_MD5" = "$GOT_MP_MD5" ]; then echo "  ok ($GOT_MP_MD5)"; else echo "  MISMATCH $MP_MD5 vs $GOT_MP_MD5"; exit 1; fi

echo "== multipart uploads list is empty after complete"
curl -sf "$BASE/$BUCKET?uploads" | head -c 300; echo

echo "== MULTIPART abort"
INIT_XML="$(curl -sf -X POST "$BASE/$BUCKET/abort-me?uploads")"
AB_ID="$(printf '%s' "$INIT_XML" | sed -n 's:.*<UploadId>\([^<]*\)</UploadId>.*:\1:p')"
curl -sf -o /dev/null -X PUT --data-binary "scrap" "$BASE/$BUCKET/abort-me?uploadId=$AB_ID&partNumber=1"
curl -sf -o /dev/null -w "  abort=%{http_code}\n" -X DELETE "$BASE/$BUCKET/abort-me?uploadId=$AB_ID"

echo "== STORAGE CLASSES"
for cls in STANDARD STANDARD_IA GLACIER_IR; do
  curl -sf -o /dev/null -X PUT -H "x-amz-storage-class: $cls" --data-binary "content-$cls" "$BASE/$BUCKET/class-${cls}.txt"
  hdr=$(curl -sfI "$BASE/$BUCKET/class-${cls}.txt" | grep -i '^x-amz-storage-class:' | sed 's/^[^:]*: *//' | tr -d '\r\n')
  if [ "$hdr" = "$cls" ]; then echo "  $cls stored+returned: ok"; else echo "  $cls FAIL (got '$hdr')"; exit 1; fi
done
bad_code=$(curl -s -o /dev/null -w '%{http_code}' -X PUT -H "x-amz-storage-class: BOGUS" --data-binary "x" "$BASE/$BUCKET/bogus.txt")
[ "$bad_code" = "400" ] && echo "  BOGUS class rejected (400)" || { echo "  BOGUS class not rejected: $bad_code"; exit 1; }
for cls in STANDARD STANDARD_IA GLACIER_IR; do
  curl -sf -o /dev/null -X DELETE "$BASE/$BUCKET/class-${cls}.txt"
done

echo "== TAGGING"
curl -sf -o /dev/null -X PUT --data-binary "tag-me" "$BASE/$BUCKET/tagged.txt"
curl -sf -o /dev/null -X PUT -H "Content-Type: application/xml" \
  --data '<Tagging><TagSet><Tag><Key>env</Key><Value>prod</Value></Tag></TagSet></Tagging>' \
  "$BASE/$BUCKET/tagged.txt?tagging"
cnt=$(curl -sfI "$BASE/$BUCKET/tagged.txt" | grep -i '^x-amz-tagging-count:' | sed 's/^[^:]*: *//' | tr -d '\r\n')
[ "$cnt" = "1" ] && echo "  tag stored: ok" || { echo "  FAIL (count=$cnt)"; exit 1; }
curl -sf -o /dev/null -X DELETE "$BASE/$BUCKET/tagged.txt?tagging"
curl -sf -o /dev/null -X DELETE "$BASE/$BUCKET/tagged.txt"

echo "== OBJECT LOCK"
curl -sf -o /dev/null -X PUT --data-binary "locked" "$BASE/$BUCKET/locked.txt"
future=$(date -u -v+1H +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -d '+1 hour' +%Y-%m-%dT%H:%M:%SZ)
curl -sf -o /dev/null -X PUT --data "<Retention><Mode>COMPLIANCE</Mode><RetainUntilDate>$future</RetainUntilDate></Retention>" "$BASE/$BUCKET/locked.txt?retention"
code=$(curl -s -o /dev/null -w '%{http_code}' -X DELETE "$BASE/$BUCKET/locked.txt")
[ "$code" = "403" ] && echo "  DELETE under retention: 403 ok" || { echo "  FAIL DELETE=$code"; exit 1; }
past=$(date -u +%Y-%m-%dT%H:%M:%SZ)
curl -sf -o /dev/null -X PUT --data "<Retention><Mode>GOVERNANCE</Mode><RetainUntilDate>$past</RetainUntilDate></Retention>" "$BASE/$BUCKET/locked.txt?retention"
curl -sf -o /dev/null -X PUT --data "<LegalHold><Status>ON</Status></LegalHold>" "$BASE/$BUCKET/locked.txt?legal-hold"
code=$(curl -s -o /dev/null -w '%{http_code}' -X DELETE "$BASE/$BUCKET/locked.txt")
[ "$code" = "403" ] && echo "  DELETE under legal hold: 403 ok" || { echo "  FAIL DELETE=$code"; exit 1; }
curl -sf -o /dev/null -X PUT --data "<LegalHold><Status>OFF</Status></LegalHold>" "$BASE/$BUCKET/locked.txt?legal-hold"
curl -sf -o /dev/null -X DELETE "$BASE/$BUCKET/locked.txt" && echo "  DELETE after hold OFF: ok"

echo "== LIFECYCLE"
curl -sf -o /dev/null -X PUT --data '<LifecycleConfiguration><Rule><ID>to-ia</ID><Status>Enabled</Status><Filter><Prefix>logs/</Prefix></Filter><Transition><Days>30</Days><StorageClass>STANDARD_IA</StorageClass></Transition></Rule></LifecycleConfiguration>' "$BASE/$BUCKET?lifecycle"
has=$(curl -sf "$BASE/$BUCKET?lifecycle" | grep -c 'to-ia')
[ "$has" = "1" ] && echo "  lifecycle rules stored: ok"
curl -sf -o /dev/null -X DELETE "$BASE/$BUCKET?lifecycle"
code=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/$BUCKET?lifecycle")
[ "$code" = "404" ] && echo "  after DELETE: 404 ok"

echo "== DELETE objects"
for k in greeting.txt logs/2026/04/a.log logs/2026/04/b.log logs/2026/05/c.log images/d.jpg big.bin mp-object; do
  curl -sf -o /dev/null -w "  %{http_code} $k\n" -X DELETE "$BASE/$BUCKET/$k"
done

echo "== VERSIONING bucket"
VBUCKET="vers-$(date +%s)"
curl -sf -o /dev/null -X PUT "$BASE/$VBUCKET"

echo "  GetBucketVersioning (empty before Enable):"
curl -sf "$BASE/$VBUCKET?versioning"; echo
echo "  PutBucketVersioning=Enabled:"
curl -sf -o /dev/null -w "    %{http_code}\n" -X PUT --data "<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>" "$BASE/$VBUCKET?versioning"
echo "  GetBucketVersioning:"
curl -sf "$BASE/$VBUCKET?versioning"; echo

echo "  PUT key 'doc' v1"
V1="$(curl -sf -o /dev/null -w '%header{x-amz-version-id}' -X PUT --data-binary "content-v1" "$BASE/$VBUCKET/doc")"
echo "    v1=$V1"
echo "  PUT key 'doc' v2"
V2="$(curl -sf -o /dev/null -w '%header{x-amz-version-id}' -X PUT --data-binary "content-v2" "$BASE/$VBUCKET/doc")"
echo "    v2=$V2"
echo "  PUT key 'doc' v3"
V3="$(curl -sf -o /dev/null -w '%header{x-amz-version-id}' -X PUT --data-binary "content-v3" "$BASE/$VBUCKET/doc")"
echo "    v3=$V3"

echo "  GET latest (expect content-v3):"
echo -n "    "; curl -sf "$BASE/$VBUCKET/doc"; echo
echo "  GET by versionId=$V1 (expect content-v1):"
echo -n "    "; curl -sf "$BASE/$VBUCKET/doc?versionId=$V1"; echo
echo "  GET by versionId=$V2 (expect content-v2):"
echo -n "    "; curl -sf "$BASE/$VBUCKET/doc?versionId=$V2"; echo

echo "  ListObjectVersions:"
curl -sf "$BASE/$VBUCKET?versions" | head -c 800; echo

echo "  DELETE latest (creates delete marker):"
curl -sf -D- -o /dev/null -X DELETE "$BASE/$VBUCKET/doc" | grep -iE 'delete-marker|version-id'

echo "  GET latest after delete-marker (expect 404):"
curl -s -o /dev/null -w "    %{http_code}\n" "$BASE/$VBUCKET/doc"

echo "  GET old version still works:"
echo -n "    "; curl -sf "$BASE/$VBUCKET/doc?versionId=$V2"; echo

echo "  ListObjectVersions now includes the delete marker:"
curl -sf "$BASE/$VBUCKET?versions" | head -c 800; echo

echo "  DELETE specific version v1:"
curl -sf -o /dev/null -w "    %{http_code}\n" -X DELETE "$BASE/$VBUCKET/doc?versionId=$V1"

echo "  ListObjects (non-versioned listing, should be empty — latest is a delete marker):"
curl -sf "$BASE/$VBUCKET?list-type=2" | head -c 400; echo

echo "  cleanup remaining versions + delete markers"
for v in $(curl -sf "$BASE/$VBUCKET?versions" | grep -oE '<VersionId>[^<]+</VersionId>' | sed 's:</*VersionId>::g'); do
  curl -sf -o /dev/null -X DELETE "$BASE/$VBUCKET/doc?versionId=$v"
done
curl -sf -o /dev/null -X DELETE "$BASE/$VBUCKET"

echo "== DELETE bucket (original)"
curl -sf -o /dev/null -w "  %{http_code}\n" -X DELETE "$BASE/$BUCKET"

echo "== smoke OK"
