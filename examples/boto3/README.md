# boto3 examples

Python equivalents of the aws-cli scripts. Boot a Strata gateway with
`examples/smoke.sh` (or any other deploy) and run a script:

```
pip install -r requirements.txt
python3 01-bucket-setup.py
```

## Scripts

| script | what it does |
| --- | --- |
| `01-bucket-setup.py` | create + list + head + delete a bucket |
| `02-multipart.py` | upload a 25 MiB file via the TransferManager (multipart) |
| `03-presign.py` | `generate_presigned_url('get_object')` and fetch via urllib |
| `04-lifecycle.py` | put + get + delete a lifecycle config |
| `05-replication.py` | enable versioning + put + get + delete a replication config |
| `06-sse-s3.py` | put_object(..., ServerSideEncryption='AES256'), verify HEAD echo + GET plaintext |
| `07-rotate-key.py` | create-user + create-access-key + RotateAccessKey via signed POST |

## Configuration

Reads the same env vars as the aws-cli examples:

| var | default |
| --- | --- |
| `STRATA_ENDPOINT` | `http://127.0.0.1:9999` |
| `STRATA_REGION` | `us-east-1` |
| `STRATA_ACCESS_KEY` | `adminAccessKey` |
| `STRATA_SECRET_KEY` | `adminSecretKey` |

## Notes

- `RotateAccessKey` is a Strata-only verb; the script signs a raw POST
  with `botocore.auth.SigV4Auth` and parses the XML response.
- `examples/smoke.sh` skips boto3 examples when `boto3` is not importable.
