# Strata examples

Copy-paste workflows for the four common S3 clients. Each subdirectory has
its own README plus 7 self-contained scripts covering the canonical
workflows:

| # | workflow |
| - | --- |
| 01 | bucket setup (create / list / head / delete) |
| 02 | multipart upload (25 MiB, force chunked, md5 round-trip) |
| 03 | presigned URL (sign + fetch via plain curl, no creds) |
| 04 | lifecycle config (put / get / delete) |
| 05 | replication setup (versioning + put / get / delete) |
| 06 | SSE-S3 (PUT with AES256, HEAD echo, GET decrypt) |
| 07 | IAM access-key rotation (create-user, RotateAccessKey, swap-verify) |

## Layout

```
examples/
  smoke.sh          # boots a fresh in-memory gateway + runs every example
  lib/common.sh     # shared shell helpers (endpoint URL, creds)
  aws-cli/          # bash + aws-cli
  boto3/            # python + boto3
  mc/               # MinIO Client (mc)
  s3cmd/            # s3cmd
```

## Running

```bash
# Boots strata server in-memory + runs every example end-to-end:
bash examples/smoke.sh

# Or point at any other Strata deployment + run a single script:
export STRATA_ENDPOINT=https://strata.example.com
export STRATA_ACCESS_KEY=...
export STRATA_SECRET_KEY=...
bash examples/aws-cli/06-sse-s3.sh
```

`smoke.sh` requires `aws-cli`. boto3 / mc / s3cmd are run per-tool when
the binary or module is available, and skipped (without failure) when it
isn't.

## Configuration

Every script reads the same env vars (defaults match `smoke.sh`):

| var | default |
| --- | --- |
| `STRATA_ENDPOINT` | `http://127.0.0.1:9999` |
| `STRATA_REGION` | `us-east-1` |
| `STRATA_ACCESS_KEY` | `adminAccessKey` |
| `STRATA_SECRET_KEY` | `adminSecretKey` (mc requires >=8 chars) |

`smoke.sh` overrides `STRATA_ENDPOINT` to its boot port (default `19999`)
and assigns the static credentials to the `[iam root]` principal so the
IAM admin verbs work.
