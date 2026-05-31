package s3api

import (
	"encoding/xml"
	"errors"
	"io"
	"net/http"

	"github.com/danchupin/strata/internal/meta"
)

type publicAccessBlockConfiguration struct {
	XMLName               xml.Name `xml:"PublicAccessBlockConfiguration"`
	BlockPublicAcls       bool     `xml:"BlockPublicAcls"`
	IgnorePublicAcls      bool     `xml:"IgnorePublicAcls"`
	BlockPublicPolicy     bool     `xml:"BlockPublicPolicy"`
	RestrictPublicBuckets bool     `xml:"RestrictPublicBuckets"`
}

// effectivePublicAccessBlock loads + parses the bucket's PublicAccessBlock
// configuration for use by the data-plane access gates. A missing config
// (ErrNoSuchPublicAccessBlock) or an empty blob both mean "no block" and
// return (nil, nil) — absence is not an error. A malformed stored blob (which
// putBucketPublicAccessBlock rejects, so should never occur) surfaces as an
// error so the gate fails closed via ErrInternal rather than silently opening.
func (s *Server) effectivePublicAccessBlock(r *http.Request, b *meta.Bucket) (*publicAccessBlockConfiguration, error) {
	blob, err := s.Meta.GetBucketPublicAccessBlock(r.Context(), b.ID)
	if err != nil {
		if errors.Is(err, meta.ErrNoSuchPublicAccessBlock) {
			return nil, nil
		}
		return nil, err
	}
	if len(blob) == 0 {
		return nil, nil
	}
	var cfg publicAccessBlockConfiguration
	if err := xml.Unmarshal(blob, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (s *Server) putBucketPublicAccessBlock(w http.ResponseWriter, r *http.Request, bucket string) {
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
	var cfg publicAccessBlockConfiguration
	if err := xml.Unmarshal(body, &cfg); err != nil {
		writeError(w, r, ErrMalformedXML)
		return
	}
	if err := s.Meta.SetBucketPublicAccessBlock(r.Context(), b.ID, body); err != nil {
		mapMetaErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) getBucketPublicAccessBlock(w http.ResponseWriter, r *http.Request, bucket string) {
	b, err := s.Meta.GetBucket(r.Context(), bucket)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	blob, err := s.Meta.GetBucketPublicAccessBlock(r.Context(), b.ID)
	if err != nil {
		if errors.Is(err, meta.ErrNoSuchPublicAccessBlock) {
			writeError(w, r, ErrNoSuchPublicAccessBlock)
			return
		}
		mapMetaErr(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(blob)
}

func (s *Server) deleteBucketPublicAccessBlock(w http.ResponseWriter, r *http.Request, bucket string) {
	b, err := s.Meta.GetBucket(r.Context(), bucket)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	if err := s.Meta.DeleteBucketPublicAccessBlock(r.Context(), b.ID); err != nil {
		mapMetaErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
