# s3cmd examples

Each script writes a temporary `.s3cfg` from the env vars in
`examples/lib/common.sh` and uses `s3cmd -c <cfg>` so the global config is
left alone.

## Scripts

| script | what it does |
| --- | --- |
| `01-bucket-setup.sh` | `s3cmd mb / ls / info / rb` |
| `02-multipart.sh` | 25 MiB upload with `--multipart-chunk-size-mb=5` |
| `03-presign.sh` | s3cmd's `signurl` only emits SigV2; Strata is SigV4-only, so delegates to `aws-cli/03-presign.sh` |
| `04-lifecycle.sh` | `s3cmd setlifecycle lifecycle.xml` (s3cmd consumes XML, not JSON) |
| `05-replication.sh` | s3cmd has no replication CLI; delegates to `aws-cli/05-replication.sh` |
| `06-sse-s3.sh` | `s3cmd --server-side-encryption put` then `s3cmd info` |
| `07-rotate-key.sh` | s3cmd has no IAM CLI; delegates to `aws-cli/07-rotate-key.sh` |

## Notes

- s3cmd's lifecycle CLI takes XML, not JSON.
- `s3cmd info` parses `x-amz-server-side-encryption` from the GET/HEAD
  response and surfaces it as `SSE: AES256`.
