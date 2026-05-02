# Sourced helper: build a temporary s3cfg pointed at the running gateway.
# Defines $S3CFG (config path) and $S3CMD (command runner) for callers.
HERE="$(cd "$(dirname "$0")" && pwd)"
. "$HERE/../lib/common.sh"

# Build a per-call config file in $TMPDIR so parallel runs don't stomp.
S3CFG="$(mktemp -t strata-s3cfg.XXXXXX)"

cat > "$S3CFG" <<EOF
[default]
host_base = ${STRATA_ENDPOINT#http://}
host_bucket = ${STRATA_ENDPOINT#http://}
access_key = $STRATA_ACCESS_KEY
secret_key = $STRATA_SECRET_KEY
use_https = False
signature_v2 = False
bucket_location = $STRATA_REGION
check_ssl_certificate = False
EOF

# Path-style is forced via host_bucket == host_base (no per-bucket subdomain).
S3CMD="s3cmd -c $S3CFG"

# Caller can `trap 'rm -f "$S3CFG"' EXIT`.
