package s3api

import (
	"encoding/xml"
	"io"
	"net/http"

	"github.com/danchupin/strata/internal/meta"
)

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
