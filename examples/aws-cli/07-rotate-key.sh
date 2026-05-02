#!/usr/bin/env bash
# Mint a user, rotate that user's access key via Strata's RotateAccessKey
# admin verb (a Strata extension), verify old key is rejected and new key
# works. Caller must be the [iam root] principal (owner=iam-root in the
# static-credentials triple).
#
# Strata's SigV4 verifier requires the x-amz-content-sha256 header on every
# signed request -- aws-cli's iam command omits that header, so we sign all
# IAM calls via curl --aws-sigv4 instead.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
. "$HERE/../lib/common.sh"

iam_signed() {
    # iam_signed <Action> [Param=Value ...]
    local action="$1"; shift
    local query="Action=$action"
    local kv
    for kv in "$@"; do query+="&$kv"; done
    curl -sf -X POST \
        --aws-sigv4 "aws:amz:$STRATA_REGION:iam" \
        --user "$STRATA_ACCESS_KEY:$STRATA_SECRET_KEY" \
        -H 'x-amz-content-sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855' \
        "$STRATA_ENDPOINT/?$query"
}

xml_field() {
    # xml_field <tag> <body>
    printf '%s' "$2" | python3 -c '
import re, sys
tag = sys.argv[1]
m = re.search(rf"<{tag}>([^<]+)</{tag}>", sys.stdin.read())
print(m.group(1) if m else "")' "$1"
}

USER="alice-$(strata_suffix)"

echo "== CreateUser $USER"
iam_signed CreateUser "UserName=$USER" >/dev/null

echo "== CreateAccessKey for $USER"
RESP="$(iam_signed CreateAccessKey "UserName=$USER")"
OLD_AK="$(xml_field AccessKeyId "$RESP")"
OLD_SK="$(xml_field SecretAccessKey "$RESP")"
[ -n "$OLD_AK" ] && [ -n "$OLD_SK" ] || { echo "create-access-key failed: $RESP"; exit 1; }
echo "  ak=$OLD_AK"

echo "== sanity: old key can list buckets"
AWS_ACCESS_KEY_ID="$OLD_AK" AWS_SECRET_ACCESS_KEY="$OLD_SK" \
    aws --endpoint-url="$STRATA_ENDPOINT" --region "$STRATA_REGION" \
    s3api list-buckets >/dev/null
echo "  ok"

echo "== RotateAccessKey"
RESP="$(iam_signed RotateAccessKey "AccessKeyId=$OLD_AK")"
NEW_AK="$(xml_field AccessKeyId "$RESP")"
NEW_SK="$(xml_field SecretAccessKey "$RESP")"
[ -n "$NEW_AK" ] && [ -n "$NEW_SK" ] && [ "$NEW_AK" != "$OLD_AK" ] \
    || { echo "rotate failed: $RESP"; exit 1; }
echo "  rotated: $OLD_AK -> $NEW_AK"

echo "== old key now rejected"
set +e
AWS_ACCESS_KEY_ID="$OLD_AK" AWS_SECRET_ACCESS_KEY="$OLD_SK" \
    aws --endpoint-url="$STRATA_ENDPOINT" --region "$STRATA_REGION" \
    s3api list-buckets >/dev/null 2>&1
RC=$?
set -e
[ $RC -ne 0 ] || { echo "old key still works"; exit 1; }
echo "  ok (rc=$RC)"

echo "== new key works"
AWS_ACCESS_KEY_ID="$NEW_AK" AWS_SECRET_ACCESS_KEY="$NEW_SK" \
    aws --endpoint-url="$STRATA_ENDPOINT" --region "$STRATA_REGION" \
    s3api list-buckets >/dev/null
echo "  ok"

iam_signed DeleteAccessKey "UserName=$USER" "AccessKeyId=$NEW_AK" >/dev/null
iam_signed DeleteUser "UserName=$USER" >/dev/null
echo "OK"
