#!/usr/bin/env bash
# s3cmd has no IAM verbs; delegate to aws-cli.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
. "$HERE/../lib/common.sh"

if ! command -v aws >/dev/null 2>&1; then
    echo "skip: aws-cli not installed (s3cmd has no IAM commands)"
    exit 0
fi
exec bash "$HERE/../aws-cli/07-rotate-key.sh"
