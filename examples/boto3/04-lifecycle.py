"""Put + get + delete a bucket lifecycle config."""
from _common import s3_client, suffix

s3 = s3_client()
bucket = f"ex-lc-{suffix()}"
s3.create_bucket(Bucket=bucket)

cfg = {
    "Rules": [
        {
            "ID": "expire-logs-after-30d",
            "Status": "Enabled",
            "Filter": {"Prefix": "logs/"},
            "Expiration": {"Days": 30},
        },
        {
            "ID": "abort-stale-multipart-after-7d",
            "Status": "Enabled",
            "Filter": {"Prefix": ""},
            "AbortIncompleteMultipartUpload": {"DaysAfterInitiation": 7},
        },
    ]
}

s3.put_bucket_lifecycle_configuration(Bucket=bucket, LifecycleConfiguration=cfg)
got = s3.get_bucket_lifecycle_configuration(Bucket=bucket)
ids = sorted(r["ID"] for r in got["Rules"])
assert ids == ["abort-stale-multipart-after-7d", "expire-logs-after-30d"], ids
print(f"rules: {ids}")

s3.delete_bucket_lifecycle(Bucket=bucket)
s3.delete_bucket(Bucket=bucket)
print("OK")
