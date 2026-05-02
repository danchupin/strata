"""Create-user, create-access-key, RotateAccessKey, verify swap.

Strata's SigV4 verifier requires the x-amz-content-sha256 header on every
signed request. botocore's iam client omits it for non-S3 services, so we
sign IAM calls manually via SigV4Auth and an explicit header.
"""
import hashlib
import re
import urllib.request

import botocore.auth
import botocore.awsrequest
import botocore.credentials
from botocore.exceptions import ClientError

from _common import ACCESS_KEY, ENDPOINT, REGION, SECRET_KEY, s3_client, suffix

EMPTY_BODY_SHA = hashlib.sha256(b"").hexdigest()


def iam_signed(action: str, **params: str) -> str:
    qs = "&".join([f"Action={action}"] + [f"{k}={v}" for k, v in params.items()])
    url = f"{ENDPOINT}/?{qs}"
    req = botocore.awsrequest.AWSRequest(method="POST", url=url, data="")
    req.headers["x-amz-content-sha256"] = EMPTY_BODY_SHA
    creds = botocore.credentials.Credentials(ACCESS_KEY, SECRET_KEY)
    botocore.auth.SigV4Auth(creds, "iam", REGION).add_auth(req)
    raw = urllib.request.Request(url, data=b"", method="POST", headers=dict(req.headers.items()))
    return urllib.request.urlopen(raw).read().decode()


def xml_field(tag: str, body: str) -> str:
    m = re.search(rf"<{tag}>([^<]+)</{tag}>", body)
    return m.group(1) if m else ""


user = f"alice-{suffix()}"

iam_signed("CreateUser", UserName=user)
print(f"created user {user}")

resp = iam_signed("CreateAccessKey", UserName=user)
old_id = xml_field("AccessKeyId", resp)
old_secret = xml_field("SecretAccessKey", resp)
assert old_id and old_secret, resp
print(f"  ak={old_id}")

s3_client(old_id, old_secret).list_buckets()
print("  old key list-buckets ok")

resp = iam_signed("RotateAccessKey", AccessKeyId=old_id)
new_id = xml_field("AccessKeyId", resp)
new_secret = xml_field("SecretAccessKey", resp)
assert new_id and new_secret and new_id != old_id, resp
print(f"  rotated -> {new_id}")

try:
    s3_client(old_id, old_secret).list_buckets()
except ClientError as e:
    print(f"  old key rejected ({e.response['Error']['Code']})")
else:
    raise SystemExit("old key still works")

s3_client(new_id, new_secret).list_buckets()
print("  new key works")

iam_signed("DeleteAccessKey", UserName=user, AccessKeyId=new_id)
iam_signed("DeleteUser", UserName=user)
print("OK")
