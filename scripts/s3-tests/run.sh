#!/usr/bin/env bash
# Runs Ceph's s3-tests compatibility suite against a live Strata gateway.
#
# Prereqs: python3, pip, git. The suite lives in a scratch dir the first run
# clones s3-tests into; subsequent runs reuse the same checkout unless
# CLEAN_SUITE=1 is set. Gateway must be running with auth=required AND the two
# pairs of credentials defined in this file listed in STRATA_STATIC_CREDENTIALS.
#
# Usage:
#   scripts/s3-tests/run.sh                  # default subset
#   S3_TESTS_FILTER=all scripts/s3-tests/run.sh
#   STRATA_ENDPOINT=http://... scripts/s3-tests/run.sh

set -euo pipefail

ENDPOINT="${STRATA_ENDPOINT:-http://127.0.0.1:9999}"
HOST="${STRATA_HOST:-127.0.0.1}"
PORT="${STRATA_PORT:-9999}"
S3TESTS_DIR="${S3TESTS_DIR:-$(mktemp -d /tmp/strata-s3tests.XXXX)/s3-tests}"
S3TESTS_REV="${S3TESTS_REV:-master}"

MAIN_AK="${MAIN_AK:-testMainAK}"
MAIN_SK="${MAIN_SK:-testMainSK}"
ALT_AK="${ALT_AK:-testAltAK}"
ALT_SK="${ALT_SK:-testAltSK}"
TENANT_AK="${TENANT_AK:-testTenantAK}"
TENANT_SK="${TENANT_SK:-testTenantSK}"

# Default subset matches features Strata currently implements. Run with
# S3_TESTS_FILTER=all to attempt everything (many will fail — expected until
# we close ROADMAP items).
DEFAULT_FILTER="test_bucket_create or test_bucket_list or test_object_write or test_object_read or test_object_delete or test_multipart or test_versioning_obj or test_bucket_list_versions"
FILTER="${S3_TESTS_FILTER:-$DEFAULT_FILTER}"
if [ "$FILTER" = "all" ]; then FILTER=""; fi

if [ ! -d "$S3TESTS_DIR" ]; then
  echo "== cloning s3-tests into $S3TESTS_DIR"
  mkdir -p "$(dirname "$S3TESTS_DIR")"
  git clone --depth=1 --branch "$S3TESTS_REV" https://github.com/ceph/s3-tests.git "$S3TESTS_DIR"
fi

cd "$S3TESTS_DIR"

if [ ! -d virtualenv ]; then
  echo "== bootstrapping virtualenv"
  python3 -m venv virtualenv
  # The upstream bootstrap script is Debian-ish; on macOS we install manually.
  ./virtualenv/bin/pip install --upgrade pip setuptools wheel >/dev/null
  ./virtualenv/bin/pip install -r requirements.txt >/dev/null
  ./virtualenv/bin/pip install -e . >/dev/null
fi

cat > s3tests.conf <<EOF
[DEFAULT]
host = $HOST
port = $PORT
is_secure = False
ssl_verify = False

[fixtures]
bucket prefix = strata-{random}-
iam name prefix = s3-tests-
iam path prefix = /s3-tests/

[s3 main]
display_name = main
user_id = main-user
email = main@strata.local
api_name = default
access_key = $MAIN_AK
secret_key = $MAIN_SK

[s3 alt]
display_name = alt
user_id = alt-user
email = alt@strata.local
access_key = $ALT_AK
secret_key = $ALT_SK

[s3 tenant]
display_name = tenant
user_id = tenant-user
email = tenant@strata.local
access_key = $TENANT_AK
secret_key = $TENANT_SK
tenant = strata-tenant

[iam]
display_name = iam
user_id = iam-user
email = iam@strata.local
access_key = $MAIN_AK
secret_key = $MAIN_SK

[iam root]
access_key = iamRootAK
secret_key = iamRootSK
user_id = iam-root
email = iam-root@strata.local

[iam alt root]
access_key = iamAltRootAK
secret_key = iamAltRootSK
user_id = iam-alt-root
email = iam-alt-root@strata.local
EOF

echo "== credentials Strata needs (paste into STRATA_STATIC_CREDENTIALS):"
echo "  ${MAIN_AK}:${MAIN_SK}:main-owner,${ALT_AK}:${ALT_SK}:alt-owner,${TENANT_AK}:${TENANT_SK}:tenant-owner"
echo

REPORT_DIR="$PWD/report"
mkdir -p "$REPORT_DIR"

PYTEST_ARGS=(--tb=short)
if [ -n "$FILTER" ]; then
  PYTEST_ARGS+=(-k "$FILTER")
fi

echo "== running s3-tests (filter='${FILTER:-ALL}')"
S3TEST_CONF=s3tests.conf ./virtualenv/bin/pytest \
  s3tests/functional \
  --junitxml="$REPORT_DIR/junit.xml" \
  "${PYTEST_ARGS[@]}" \
  2>&1 | tee "$REPORT_DIR/pytest.log" || true

echo
echo "== summary"
if command -v python3 >/dev/null && [ -s "$REPORT_DIR/junit.xml" ]; then
  python3 - <<'PY'
import xml.etree.ElementTree as ET, os
tree = ET.parse(os.path.join(os.environ['PWD'], 'report', 'junit.xml'))
root = tree.getroot()
suites = root if root.tag == 'testsuites' else [root]
total = errors = failures = skipped = 0
for s in suites.iter('testsuite') if hasattr(suites, 'iter') else suites:
    total += int(s.get('tests', 0))
    errors += int(s.get('errors', 0))
    failures += int(s.get('failures', 0))
    skipped += int(s.get('skipped', 0))
passed = total - errors - failures - skipped
print(f"tests={total}  passed={passed}  failed={failures}  errors={errors}  skipped={skipped}")
if total:
    print(f"pass rate: {passed/total*100:.1f}%")
PY
fi

echo
echo "report:        $REPORT_DIR/junit.xml"
echo "full log:      $REPORT_DIR/pytest.log"
echo "checkout:      $S3TESTS_DIR (reuse via S3TESTS_DIR=$S3TESTS_DIR)"
