package racetest

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
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

// AWS streaming-payload constants. Copies of internal/auth values; we
// duplicate them here so the racetest package stays free of a cyclic
// dep on internal/auth.
const (
	streamingPayloadHash = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD"
	chunkPayloadAlgo     = "AWS4-HMAC-SHA256-PAYLOAD"
	emptyBodyHashHex     = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	sigTerminator        = "aws4_request"
)

// signStreaming SigV4-signs the request with payload-hash =
// STREAMING-AWS4-HMAC-SHA256-PAYLOAD, then replaces req.Body with an
// aws-chunked encoding of body whose chunks are chained via
// computeChunkSignature. Exercises the gateway's streaming verifier
// (internal/auth/streaming.go) — counterpart to the pre-computed-SHA
// path covered by sign().
//
// Encoded body shape (one data chunk + terminator):
//
//	<sizeHex>;chunk-signature=<seed-sig>\r\n
//	<body>\r\n
//	0;chunk-signature=<sig-of-empty-chunk-chained-from-seed>\r\n
//
// Encoded length is deterministic from len(body) — chunk-signature is
// always 64 hex chars — so we can pre-compute Content-Length before
// signing, which keeps the signed Content-Length header consistent
// with the wire body.
func (s *signer) signStreaming(ctx context.Context, req *http.Request, body []byte) error {
	decoded := len(body)
	encodedLen := streamingEncodedLen(decoded)

	req.Header.Set("X-Amz-Content-Sha256", streamingPayloadHash)
	req.Header.Set("X-Amz-Decoded-Content-Length", strconv.Itoa(decoded))
	req.Header.Set("Content-Length", strconv.Itoa(encodedLen))
	req.ContentLength = int64(encodedLen)

	now := time.Now().UTC()
	if err := s.v4.SignHTTP(ctx, s.creds, req, streamingPayloadHash, s.svc, s.region, now); err != nil {
		return err
	}

	seedSig := parseAuthSignature(req.Header.Get("Authorization"))
	if seedSig == "" {
		return fmt.Errorf("racetest: missing seed signature in Authorization header")
	}
	reqDate := req.Header.Get("X-Amz-Date")
	if len(reqDate) < 8 {
		return fmt.Errorf("racetest: missing X-Amz-Date after signing")
	}
	date := reqDate[:8]
	signingKey := deriveSigningKey(s.creds.SecretAccessKey, date, s.region, s.svc)
	scope := date + "/" + s.region + "/" + s.svc + "/" + sigTerminator

	encoded := encodeStreamingBody(body, signingKey, reqDate, scope, seedSig)
	if len(encoded) != encodedLen {
		return fmt.Errorf("racetest: streaming encoded length mismatch (got %d want %d)",
			len(encoded), encodedLen)
	}
	req.Body = io.NopCloser(bytes.NewReader(encoded))
	return nil
}

// streamingEncodedLen returns the total wire byte count of an
// aws-chunked body with a single data chunk of decoded length n plus
// the zero-length terminator chunk. chunk-signature is always 64 hex
// chars so the answer is deterministic from n.
func streamingEncodedLen(n int) int {
	sizeHex := strconv.FormatInt(int64(n), 16)
	const sigPart = ";chunk-signature=" // 17 bytes
	const sigLen = 64
	const crlf = 2

	// data chunk header: "<sizeHex>;chunk-signature=<sig>\r\n"
	headerLen := len(sizeHex) + len(sigPart) + sigLen + crlf
	dataLen := n
	dataTrail := 0
	if n > 0 {
		dataTrail = crlf
	}
	// terminator: "0;chunk-signature=<sig>\r\n"
	termLen := 1 + len(sigPart) + sigLen + crlf
	return headerLen + dataLen + dataTrail + termLen
}

// encodeStreamingBody emits the aws-chunked body. The seed signature is
// the seed for the first chunk; each subsequent chunk chains from the
// previous chunk's signature.
func encodeStreamingBody(body, signingKey []byte, reqDate, scope, seedSig string) []byte {
	var buf bytes.Buffer
	prev := seedSig

	dataSig := computeChunkSignature(signingKey, reqDate, scope, prev, body)
	fmt.Fprintf(&buf, "%x;chunk-signature=%s\r\n", len(body), dataSig)
	if len(body) > 0 {
		buf.Write(body)
		buf.WriteString("\r\n")
	}
	prev = dataSig

	finalSig := computeChunkSignature(signingKey, reqDate, scope, prev, nil)
	fmt.Fprintf(&buf, "0;chunk-signature=%s\r\n", finalSig)
	return buf.Bytes()
}

// parseAuthSignature pulls the Signature= field out of an
// "AWS4-HMAC-SHA256 Credential=...,SignedHeaders=...,Signature=..." header.
// Returns "" if not present.
func parseAuthSignature(h string) string {
	for _, part := range strings.Split(h, ",") {
		part = strings.TrimSpace(part)
		if v, ok := strings.CutPrefix(part, "Signature="); ok {
			return v
		}
	}
	return ""
}

// deriveSigningKey is the AWS-spec key derivation; mirrors
// internal/auth.deriveSigningKey so we don't import the auth package
// (avoids surfacing internal/auth as a public dep of racetest).
func deriveSigningKey(secret, date, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), []byte(date))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	return hmacSHA256(kService, []byte(sigTerminator))
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func sha256HexBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// computeChunkSignature implements the streaming-SigV4 chained-HMAC
// formula (AWS4-HMAC-SHA256-PAYLOAD). prevSig is the seed signature
// for the first chunk; chunks chain by prevSig=previous chunk sig.
func computeChunkSignature(signingKey []byte, reqDate, scope, prevSig string, data []byte) string {
	sts := chunkPayloadAlgo + "\n" +
		reqDate + "\n" +
		scope + "\n" +
		prevSig + "\n" +
		emptyBodyHashHex + "\n" +
		sha256HexBytes(data)
	return hex.EncodeToString(hmacSHA256(signingKey, []byte(sts)))
}
