"""Generate a presigned GET URL and fetch via urllib (no creds)."""
import urllib.request

from _common import s3_client, suffix

s3 = s3_client()
bucket = f"ex-pre-{suffix()}"
s3.create_bucket(Bucket=bucket)

body = b"presigned content"
s3.put_object(Bucket=bucket, Key="secret.txt", Body=body)

url = s3.generate_presigned_url(
    "get_object", Params={"Bucket": bucket, "Key": "secret.txt"}, ExpiresIn=60
)
print(f"presigned: {url}")

with urllib.request.urlopen(url) as resp:
    got = resp.read()
assert got == body, (got, body)
print("plaintext round-trip ok")

s3.delete_object(Bucket=bucket, Key="secret.txt")
s3.delete_bucket(Bucket=bucket)
print("OK")
