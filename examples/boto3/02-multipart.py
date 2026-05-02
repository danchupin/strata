"""Upload 25 MiB via the boto3 TransferManager (auto-multipart) + verify md5."""
import hashlib
import os
import tempfile

from boto3.s3.transfer import TransferConfig

from _common import s3_client, suffix

s3 = s3_client()
bucket = f"ex-mp-{suffix()}"
s3.create_bucket(Bucket=bucket)

cfg = TransferConfig(multipart_threshold=8 * 1024 * 1024, multipart_chunksize=8 * 1024 * 1024)

with tempfile.TemporaryDirectory() as tmp:
    src = os.path.join(tmp, "big.bin")
    with open(src, "wb") as f:
        f.write(os.urandom(25 * 1024 * 1024))
    orig = hashlib.md5(open(src, "rb").read()).hexdigest()

    s3.upload_file(src, bucket, "big.bin", Config=cfg)
    print("uploaded 25 MiB via multipart")

    dst = os.path.join(tmp, "got.bin")
    s3.download_file(bucket, "big.bin", dst, Config=cfg)
    got = hashlib.md5(open(dst, "rb").read()).hexdigest()
    assert orig == got, f"md5 mismatch {orig} vs {got}"
    print(f"md5 ok ({got})")

s3.delete_object(Bucket=bucket, Key="big.bin")
s3.delete_bucket(Bucket=bucket)
print("OK")
