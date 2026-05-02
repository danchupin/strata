# Shared helpers for examples/. Source from each example script.
#
# Reads:
#   STRATA_ENDPOINT       (default http://127.0.0.1:9999)
#   STRATA_REGION         (default us-east-1)
#   STRATA_ACCESS_KEY     (default adminAK)
#   STRATA_SECRET_KEY     (default adminSK)
#
# All scripts expect a strata server already running. examples/smoke.sh
# launches a fresh in-memory server with creds + master key configured;
# you can also point at any other Strata deploy by exporting STRATA_ENDPOINT.

export STRATA_ENDPOINT="${STRATA_ENDPOINT:-http://127.0.0.1:9999}"
export STRATA_REGION="${STRATA_REGION:-us-east-1}"
export STRATA_ACCESS_KEY="${STRATA_ACCESS_KEY:-adminAccessKey}"
export STRATA_SECRET_KEY="${STRATA_SECRET_KEY:-adminSecretKey}"

# Bridge into AWS_* so aws-cli + boto3 + s3cmd pick them up.
export AWS_ACCESS_KEY_ID="$STRATA_ACCESS_KEY"
export AWS_SECRET_ACCESS_KEY="$STRATA_SECRET_KEY"
export AWS_DEFAULT_REGION="$STRATA_REGION"
export AWS_EC2_METADATA_DISABLED=true

aws_strata() {
    aws --endpoint-url="$STRATA_ENDPOINT" --no-verify-ssl "$@"
}

# Random suffix for unique bucket / object names per run.
strata_suffix() {
    date +%s%N | tail -c 9
}

# Wait for /healthz, max 30s.
strata_wait_ready() {
    for i in $(seq 1 60); do
        if curl -sf -o /dev/null "$STRATA_ENDPOINT/healthz"; then
            return 0
        fi
        sleep 0.5
    done
    echo "strata server not ready at $STRATA_ENDPOINT" >&2
    return 1
}

md5of() {
    if command -v md5 >/dev/null 2>&1; then
        md5 -q "$1"
    else
        md5sum "$1" | awk '{print $1}'
    fi
}
