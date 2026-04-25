package s3api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/danchupin/strata/internal/meta"
)

func (s *Server) putBucketPolicy(w http.ResponseWriter, r *http.Request, bucket string) {
	b, err := s.Meta.GetBucket(r.Context(), bucket)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 20<<10))
	if err != nil || len(body) == 0 {
		writeError(w, r, ErrInvalidArgument)
		return
	}
	var probe map[string]any
	if err := json.Unmarshal(body, &probe); err != nil {
		writeError(w, r, ErrInvalidArgument)
		return
	}
	if err := s.Meta.SetBucketPolicy(r.Context(), b.ID, body); err != nil {
		mapMetaErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) getBucketPolicy(w http.ResponseWriter, r *http.Request, bucket string) {
	b, err := s.Meta.GetBucket(r.Context(), bucket)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	blob, err := s.Meta.GetBucketPolicy(r.Context(), b.ID)
	if err != nil {
		if errors.Is(err, meta.ErrNoSuchBucketPolicy) {
			writeError(w, r, ErrNoSuchBucketPolicy)
			return
		}
		mapMetaErr(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(blob)
}

func (s *Server) deleteBucketPolicy(w http.ResponseWriter, r *http.Request, bucket string) {
	b, err := s.Meta.GetBucket(r.Context(), bucket)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	if err := s.Meta.DeleteBucketPolicy(r.Context(), b.ID); err != nil {
		mapMetaErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
