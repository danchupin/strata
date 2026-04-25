package s3api

import (
	"crypto/md5"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"io"
	"net/http"

	"github.com/danchupin/strata/internal/meta"
)

const sseAlgorithmAES256 = "AES256"

const (
	hdrSSECAlgorithm = "x-amz-server-side-encryption-customer-algorithm"
	hdrSSECKey       = "x-amz-server-side-encryption-customer-key"
	hdrSSECKeyMD5    = "x-amz-server-side-encryption-customer-key-MD5"

	hdrCopySSECAlgorithm = "x-amz-copy-source-server-side-encryption-customer-algorithm"
	hdrCopySSECKey       = "x-amz-copy-source-server-side-encryption-customer-key"
	hdrCopySSECKeyMD5    = "x-amz-copy-source-server-side-encryption-customer-key-MD5"
)

// ssecHeaders captures parsed and validated SSE-C customer key headers. Empty
// when the request did not supply the customer-key triple.
type ssecHeaders struct {
	Algorithm string
	KeyMD5    string
	Present   bool
}

// parseSSECHeaders reads the regular x-amz-server-side-encryption-customer-*
// triple. Returns (parsed, apiErr, ok). When ok=false the caller must stop and
// write apiErr. A request with no SSE-C headers returns Present=false, ok=true.
func parseSSECHeaders(r *http.Request) (ssecHeaders, APIError, bool) {
	return parseSSECTriple(r, hdrSSECAlgorithm, hdrSSECKey, hdrSSECKeyMD5)
}

// parseCopySourceSSECHeaders reads the x-amz-copy-source-server-side-encryption-customer-*
// mirror, used by CopyObject for an SSE-C-encrypted source.
func parseCopySourceSSECHeaders(r *http.Request) (ssecHeaders, APIError, bool) {
	return parseSSECTriple(r, hdrCopySSECAlgorithm, hdrCopySSECKey, hdrCopySSECKeyMD5)
}

func parseSSECTriple(r *http.Request, algoHdr, keyHdr, keyMD5Hdr string) (ssecHeaders, APIError, bool) {
	algo := r.Header.Get(algoHdr)
	rawKey := r.Header.Get(keyHdr)
	keyMD5 := r.Header.Get(keyMD5Hdr)
	if algo == "" && rawKey == "" && keyMD5 == "" {
		return ssecHeaders{}, APIError{}, true
	}
	if algo == "" || rawKey == "" || keyMD5 == "" {
		return ssecHeaders{}, ErrInvalidRequest, false
	}
	if algo != sseAlgorithmAES256 {
		return ssecHeaders{}, ErrInvalidEncryptionAlgorithm, false
	}
	keyBytes, err := base64.StdEncoding.DecodeString(rawKey)
	if err != nil || len(keyBytes) != 32 {
		return ssecHeaders{}, ErrInvalidArgument, false
	}
	expected := md5.Sum(keyBytes)
	if base64.StdEncoding.EncodeToString(expected[:]) != keyMD5 {
		return ssecHeaders{}, ErrInvalidDigest, false
	}
	return ssecHeaders{Algorithm: algo, KeyMD5: keyMD5, Present: true}, APIError{}, true
}

// requireSSECMatch enforces that a GetObject/HeadObject request supplies SSE-C
// headers matching the persisted KeyMD5 on the stored object. Caller invokes
// only when the object actually carries SSE-C metadata.
func requireSSECMatch(r *http.Request, storedKeyMD5 string) (APIError, bool) {
	hdrs, apiErr, ok := parseSSECHeaders(r)
	if !ok {
		return apiErr, false
	}
	if !hdrs.Present {
		return ErrSSECRequired, false
	}
	if hdrs.KeyMD5 != storedKeyMD5 {
		return ErrSSECKeyMismatch, false
	}
	return APIError{}, true
}

type sseRule struct {
	XMLName xml.Name `xml:"Rule"`
	Apply   *struct {
		XMLName        xml.Name `xml:"ApplyServerSideEncryptionByDefault"`
		SSEAlgorithm   string   `xml:"SSEAlgorithm"`
		KMSMasterKeyID string   `xml:"KMSMasterKeyID,omitempty"`
	} `xml:"ApplyServerSideEncryptionByDefault"`
	BucketKeyEnabled bool `xml:"BucketKeyEnabled,omitempty"`
}

type serverSideEncryptionConfiguration struct {
	XMLName xml.Name  `xml:"ServerSideEncryptionConfiguration"`
	Rules   []sseRule `xml:"Rule"`
}

func (s *Server) putBucketEncryption(w http.ResponseWriter, r *http.Request, bucket string) {
	b, err := s.Meta.GetBucket(r.Context(), bucket)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil || len(body) == 0 {
		writeError(w, r, ErrMalformedXML)
		return
	}
	var cfg serverSideEncryptionConfiguration
	if err := xml.Unmarshal(body, &cfg); err != nil || len(cfg.Rules) == 0 {
		writeError(w, r, ErrMalformedXML)
		return
	}
	for _, rule := range cfg.Rules {
		if rule.Apply == nil {
			writeError(w, r, ErrMalformedXML)
			return
		}
		switch rule.Apply.SSEAlgorithm {
		case sseAlgorithmAES256:
		case "aws:kms", "aws:kms:dsse":
			writeError(w, r, ErrKMSNotImplemented)
			return
		default:
			writeError(w, r, ErrInvalidEncryptionAlgorithm)
			return
		}
	}
	if err := s.Meta.SetBucketEncryption(r.Context(), b.ID, body); err != nil {
		mapMetaErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) getBucketEncryption(w http.ResponseWriter, r *http.Request, bucket string) {
	b, err := s.Meta.GetBucket(r.Context(), bucket)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	blob, err := s.Meta.GetBucketEncryption(r.Context(), b.ID)
	if err != nil {
		if errors.Is(err, meta.ErrNoSuchEncryption) {
			writeError(w, r, ErrNoSuchEncryption)
			return
		}
		mapMetaErr(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(blob)
}

func (s *Server) deleteBucketEncryption(w http.ResponseWriter, r *http.Request, bucket string) {
	b, err := s.Meta.GetBucket(r.Context(), bucket)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	if err := s.Meta.DeleteBucketEncryption(r.Context(), b.ID); err != nil {
		mapMetaErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// resolveSSE picks the SSE algorithm for a new object: explicit request header
// wins, falling back to the bucket default if configured. Returns (algo, apiErr,
// ok). When ok=false the caller must stop and write apiErr.
func (s *Server) resolveSSE(r *http.Request, b *meta.Bucket) (string, APIError, bool) {
	if hdr := r.Header.Get("x-amz-server-side-encryption"); hdr != "" {
		return validateSSEAlgorithm(hdr)
	}
	blob, err := s.Meta.GetBucketEncryption(r.Context(), b.ID)
	if err != nil {
		return "", APIError{}, true
	}
	algo := defaultSSEAlgorithm(blob)
	return algo, APIError{}, true
}

func validateSSEAlgorithm(algo string) (string, APIError, bool) {
	switch algo {
	case sseAlgorithmAES256:
		return algo, APIError{}, true
	case "aws:kms", "aws:kms:dsse":
		return "", ErrKMSNotImplemented, false
	default:
		return "", ErrInvalidEncryptionAlgorithm, false
	}
}

func defaultSSEAlgorithm(blob []byte) string {
	var cfg serverSideEncryptionConfiguration
	if err := xml.Unmarshal(blob, &cfg); err != nil {
		return ""
	}
	for _, rule := range cfg.Rules {
		if rule.Apply != nil && rule.Apply.SSEAlgorithm != "" {
			return rule.Apply.SSEAlgorithm
		}
	}
	return ""
}
