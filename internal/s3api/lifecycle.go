package s3api

import (
	"errors"
	"io"
	"net/http"

	"github.com/danchupin/strata/internal/meta"
)

func (s *Server) putBucketLifecycle(w http.ResponseWriter, r *http.Request, bucket string) {
	b, err := s.Meta.GetBucket(r.Context(), bucket)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, r, ErrMalformedXML)
		return
	}
	if err := s.Meta.SetBucketLifecycle(r.Context(), b.ID, body); err != nil {
		mapMetaErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) getBucketLifecycle(w http.ResponseWriter, r *http.Request, bucket string) {
	b, err := s.Meta.GetBucket(r.Context(), bucket)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	rules, err := s.Meta.GetBucketLifecycle(r.Context(), b.ID)
	if err != nil {
		if errors.Is(err, meta.ErrNoSuchLifecycle) {
			writeError(w, r, ErrNoSuchLifecycleConfiguration)
			return
		}
		mapMetaErr(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(rules)
}

func (s *Server) deleteBucketLifecycle(w http.ResponseWriter, r *http.Request, bucket string) {
	b, err := s.Meta.GetBucket(r.Context(), bucket)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	if err := s.Meta.DeleteBucketLifecycle(r.Context(), b.ID); err != nil {
		mapMetaErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
