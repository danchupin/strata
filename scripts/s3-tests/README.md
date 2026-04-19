# S3 compatibility suite

This directory wires Strata up against [Ceph's `s3-tests`][1] — the de-facto
compatibility test suite for any S3-like object gateway. Running it gives an
honest, numeric compatibility score against a broad spectrum of S3 behaviour.

[1]: https://github.com/ceph/s3-tests

## What it covers

The suite has hundreds of tests over bucket ops, object ops, ACLs, versioning,
multipart, object lock, lifecycle, CORS, website hosting, replication, and
more. Many of these cover S3 surface we do not yet implement — those will
appear as failures in the report and map back to P-items in `ROADMAP.md`.

## Prereqs

- Full Strata stack running (`make up-all`), gateway with `auth=required`.
- Python 3.9+, `git`, `pip`.
- Credentials mounted into the gateway via `STRATA_STATIC_CREDENTIALS`. The
  runner prints the exact string to use.

## Run

```bash
# Pick two + one tenant creds that the suite expects.
export MAIN_AK=testMainAK MAIN_SK=testMainSK
export ALT_AK=testAltAK   ALT_SK=testAltSK
export TENANT_AK=testTenantAK TENANT_SK=testTenantSK

# Start the gateway with those creds.
STRATA_AUTH_MODE=required \
  STRATA_STATIC_CREDENTIALS="$MAIN_AK:$MAIN_SK:main-owner,$ALT_AK:$ALT_SK:alt-owner,$TENANT_AK:$TENANT_SK:tenant-owner" \
  docker compose -f deploy/docker/docker-compose.yml up -d --force-recreate gateway

# Run the default subset (what Strata actually implements today).
scripts/s3-tests/run.sh

# Or run the full suite.
S3_TESTS_FILTER=all scripts/s3-tests/run.sh

# Or pick a specific test pattern.
S3_TESTS_FILTER=test_multipart scripts/s3-tests/run.sh
```

## Output

- `report/pytest.log` — full pytest output.
- `report/junit.xml` — machine-readable results, parsed at the end into a
  one-line summary.
- A `pass rate: X%` line on stdout that is the number you cite when claiming
  "Strata is N% S3-compatible".

## Baseline (as of first run)

Against commit `HEAD` with `auth=required`, default subset (filter matches
`test_bucket_create or test_bucket_list or test_object_write or
test_object_read or test_object_delete or test_multipart or test_versioning_obj
or test_bucket_list_versions`):

```
tests=177  passed=11  failed=8  errors=158  skipped=0
pass rate: 6.2%
```

Of the 177 collected, only 19 actually executed (setup succeeded); the
remaining 158 failed at collection because their fixtures require
not-yet-implemented features (IAM, bucket ownership, checksums, etc.).
**Of the 19 that ran, 11 passed → 58% pass rate on the executable
subset.** The 8 real FAILs are in `test_headers.py` covering SigV2 /
bad-auth edge cases we deliberately don't implement.

Full suite (no filter):

```
tests=1046  passed=3  failed=55  errors=983  skipped=5
```

The large `errors` bucket reflects how much of S3 we don't implement yet
— ACLs, CORS, IAM, bucket policies, website hosting, S3 Select,
replication, checksums, etc. Each P-item closed in `ROADMAP.md` should
shift some fraction of those errors into either pass or fail.

## Interpreting failures

Failures fall into three buckets:

1. **Not-yet-implemented feature** (e.g. `test_bucket_acl_*`, `test_cors_*`).
   These correspond to open P-items in `ROADMAP.md`. Expected, not a bug.
2. **Wrong behaviour on something we do implement.** These are real bugs —
   file an issue or fix directly.
3. **Suite assumes RGW-specific behaviour** (e.g. specific error codes,
   admin API). These are architectural drift, not correctness failures.

The suite supports `--shard-id/--num-shards` for parallelism; useful for CI.
