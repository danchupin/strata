#!/usr/bin/env bash
# `s3cmd signurl` only emits SigV2 URLs and Strata only verifies SigV4
# presigned URLs, so delegate to aws-cli (which signs with SigV4).
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
. "$HERE/../lib/common.sh"

if ! command -v aws >/dev/null 2>&1; then
    echo "skip: aws-cli not installed (s3cmd signurl is SigV2 only)"
    exit 0
fi
exec bash "$HERE/../aws-cli/03-presign.sh"
