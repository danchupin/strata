package s3api

import (
	"encoding/xml"
	"errors"
	"io"
	"net/http"

	"github.com/danchupin/strata/internal/meta"
)

type ownershipControlsRule struct {
	XMLName           xml.Name `xml:"Rule"`
	ObjectOwnership   string   `xml:"ObjectOwnership"`
}

type ownershipControlsConfiguration struct {
	XMLName xml.Name                `xml:"OwnershipControls"`
	Rules   []ownershipControlsRule `xml:"Rule"`
}

func (s *Server) putBucketOwnershipControls(w http.ResponseWriter, r *http.Request, bucket string) {
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
	var cfg ownershipControlsConfiguration
	if err := xml.Unmarshal(body, &cfg); err != nil || len(cfg.Rules) == 0 {
		writeError(w, r, ErrMalformedXML)
		return
	}
	for _, rule := range cfg.Rules {
		switch rule.ObjectOwnership {
		case "BucketOwnerPreferred", "ObjectWriter", "BucketOwnerEnforced":
		default:
			writeError(w, r, ErrInvalidArgument)
			return
		}
	}
	if err := s.Meta.SetBucketOwnershipControls(r.Context(), b.ID, body); err != nil {
		mapMetaErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) getBucketOwnershipControls(w http.ResponseWriter, r *http.Request, bucket string) {
	b, err := s.Meta.GetBucket(r.Context(), bucket)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	blob, err := s.Meta.GetBucketOwnershipControls(r.Context(), b.ID)
	if err != nil {
		if errors.Is(err, meta.ErrNoSuchOwnershipControls) {
			writeError(w, r, ErrNoSuchOwnershipControls)
			return
		}
		mapMetaErr(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(blob)
}

func (s *Server) deleteBucketOwnershipControls(w http.ResponseWriter, r *http.Request, bucket string) {
	b, err := s.Meta.GetBucket(r.Context(), bucket)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	if err := s.Meta.DeleteBucketOwnershipControls(r.Context(), b.ID); err != nil {
		mapMetaErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
