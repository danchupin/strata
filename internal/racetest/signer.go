package racetest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

// signer holds an aws-sdk-go-v2 SigV4 signer + the static credentials
// the strata-racecheck binary was launched with. Empty AccessKey turns
// signing off entirely; the caller may still hand the signer to every
// request and the wrapper short-circuits.
type signer struct {
	creds  aws.Credentials
	region string
	svc    string
	v4     *v4.Signer
}

// newSigner returns nil when accessKey is empty (anonymous mode). The
// returned signer is cheap to copy and safe to share across goroutines:
// v4.Signer is stateless after construction.
func newSigner(accessKey, secretKey, region string) *signer {
	if accessKey == "" {
		return nil
	}
	if region == "" {
		region = "us-east-1"
	}
	return &signer{
		creds:  aws.Credentials{AccessKeyID: accessKey, SecretAccessKey: secretKey},
		region: region,
		svc:    "s3",
		v4:     v4.NewSigner(),
	}
}

// sign hashes the request body (or the empty-body sentinel hash) and
// SigV4-signs the request in place. body may be nil for GET/DELETE; for
// PUT/POST the caller passes the bytes that will be sent so the
// x-amz-content-sha256 header matches the wire payload.
//
// The aws-sdk-go-v2 signer requires the request body to be readable
// after signing, so we hand it a fresh bytes.Reader on the request.
func (s *signer) sign(ctx context.Context, req *http.Request, body []byte) error {
	if body != nil {
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))
	}
	hash := sha256.Sum256(body)
	payloadHash := hex.EncodeToString(hash[:])
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	return s.v4.SignHTTP(ctx, s.creds, req, payloadHash, s.svc, s.region, time.Now().UTC())
}
