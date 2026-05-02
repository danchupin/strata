#!/usr/bin/env bash
# mc admin user APIs are MinIO-specific (require admin AccessKey on a MinIO
# server) and are not implemented by Strata. The IAM `?Action=` admin verbs
# Strata supports are AWS-shaped, so we delegate this example to aws-cli.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
. "$HERE/../lib/common.sh"

if ! command -v aws >/dev/null 2>&1; then
    echo "skip: aws-cli not installed (RotateAccessKey is an IAM action; mc has no equivalent)"
    exit 0
fi
exec bash "$HERE/../aws-cli/07-rotate-key.sh"
