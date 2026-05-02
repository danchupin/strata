package s3api

import (
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/danchupin/strata/internal/meta"
)

const maxBucketTags = 50

func (s *Server) putObjectTagging(w http.ResponseWriter, r *http.Request, b *meta.Bucket, key string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		writeError(w, r, ErrMalformedXML)
		return
	}
	var doc tagging
	if err := xml.Unmarshal(body, &doc); err != nil {
		writeError(w, r, ErrMalformedXML)
		return
	}
	tags := make(map[string]string, len(doc.TagSet.Tags))
	for _, t := range doc.TagSet.Tags {
		tags[t.Key] = t.Value
	}
	if err := s.Meta.SetObjectTags(r.Context(), b.ID, key, r.URL.Query().Get("versionId"), tags); err != nil {
		mapMetaErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) getObjectTagging(w http.ResponseWriter, r *http.Request, b *meta.Bucket, key string) {
	tags, err := s.Meta.GetObjectTags(r.Context(), b.ID, key, r.URL.Query().Get("versionId"))
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	resp := tagging{}
	for k, v := range tags {
		resp.TagSet.Tags = append(resp.TagSet.Tags, tagEntry{Key: k, Value: v})
	}
	writeXML(w, http.StatusOK, resp)
}

func (s *Server) deleteObjectTagging(w http.ResponseWriter, r *http.Request, b *meta.Bucket, key string) {
	if err := s.Meta.DeleteObjectTags(r.Context(), b.ID, key, r.URL.Query().Get("versionId")); err != nil {
		mapMetaErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) putBucketTagging(w http.ResponseWriter, r *http.Request, bucket string) {
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
	if strings.TrimSpace(string(body)) == "" {
		if err := s.Meta.DeleteBucketTagging(r.Context(), b.ID); err != nil {
			mapMetaErr(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var doc tagging
	if err := xml.Unmarshal(body, &doc); err != nil {
		writeError(w, r, ErrMalformedXML)
		return
	}
	if len(doc.TagSet.Tags) > maxBucketTags {
		writeError(w, r, ErrInvalidTag)
		return
	}
	seen := make(map[string]struct{}, len(doc.TagSet.Tags))
	for _, t := range doc.TagSet.Tags {
		if t.Key == "" {
			writeError(w, r, ErrInvalidTag)
			return
		}
		if _, dup := seen[t.Key]; dup {
			writeError(w, r, ErrInvalidTag)
			return
		}
		seen[t.Key] = struct{}{}
	}
	if err := s.Meta.SetBucketTagging(r.Context(), b.ID, body); err != nil {
		mapMetaErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) getBucketTagging(w http.ResponseWriter, r *http.Request, bucket string) {
	b, err := s.Meta.GetBucket(r.Context(), bucket)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	blob, err := s.Meta.GetBucketTagging(r.Context(), b.ID)
	if err != nil {
		if errors.Is(err, meta.ErrNoSuchTagSet) {
			writeError(w, r, ErrNoSuchTagSet)
			return
		}
		mapMetaErr(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(blob)
}

func (s *Server) deleteBucketTagging(w http.ResponseWriter, r *http.Request, bucket string) {
	b, err := s.Meta.GetBucket(r.Context(), bucket)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	if err := s.Meta.DeleteBucketTagging(r.Context(), b.ID); err != nil {
		mapMetaErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
