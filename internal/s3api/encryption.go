package s3api

import (
	"encoding/xml"
	"errors"
	"io"
	"net/http"

	"github.com/danchupin/strata/internal/meta"
)

const sseAlgorithmAES256 = "AES256"

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
