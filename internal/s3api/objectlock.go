package s3api

import (
	"encoding/xml"
	"io"
	"net/http"
	"time"

	"github.com/danchupin/strata/internal/meta"
)

func (s *Server) putObjectRetention(w http.ResponseWriter, r *http.Request, b *meta.Bucket, key string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		writeError(w, r, ErrMalformedXML)
		return
	}
	var doc retentionConfig
	if err := xml.Unmarshal(body, &doc); err != nil {
		writeError(w, r, ErrMalformedXML)
		return
	}
	var until time.Time
	if doc.RetainUntilDate != "" {
		until, err = time.Parse(time.RFC3339, doc.RetainUntilDate)
		if err != nil {
			writeError(w, r, ErrInvalidArgument)
			return
		}
	}
	mode := doc.Mode
	if mode != "" && mode != meta.LockModeGovernance && mode != meta.LockModeCompliance {
		writeError(w, r, ErrInvalidArgument)
		return
	}
	if err := s.Meta.SetObjectRetention(r.Context(), b.ID, key, r.URL.Query().Get("versionId"), mode, until); err != nil {
		mapMetaErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) getObjectRetention(w http.ResponseWriter, r *http.Request, b *meta.Bucket, key string) {
	o, err := s.Meta.GetObject(r.Context(), b.ID, key, r.URL.Query().Get("versionId"))
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	resp := retentionConfig{Mode: o.RetainMode}
	if !o.RetainUntil.IsZero() {
		resp.RetainUntilDate = o.RetainUntil.UTC().Format(time.RFC3339)
	}
	writeXML(w, http.StatusOK, resp)
}

func (s *Server) putObjectLegalHold(w http.ResponseWriter, r *http.Request, b *meta.Bucket, key string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		writeError(w, r, ErrMalformedXML)
		return
	}
	var doc legalHoldConfig
	if err := xml.Unmarshal(body, &doc); err != nil {
		writeError(w, r, ErrMalformedXML)
		return
	}
	on := doc.Status == "ON"
	if err := s.Meta.SetObjectLegalHold(r.Context(), b.ID, key, r.URL.Query().Get("versionId"), on); err != nil {
		mapMetaErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) getObjectLegalHold(w http.ResponseWriter, r *http.Request, b *meta.Bucket, key string) {
	o, err := s.Meta.GetObject(r.Context(), b.ID, key, r.URL.Query().Get("versionId"))
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	resp := legalHoldConfig{Status: "OFF"}
	if o.LegalHold {
		resp.Status = "ON"
	}
	writeXML(w, http.StatusOK, resp)
}

func objectLockBlocksDelete(o *meta.Object, bypassGovernance bool) bool {
	if o.LegalHold {
		return true
	}
	if o.RetainUntil.IsZero() || !o.RetainUntil.After(time.Now()) {
		return false
	}
	if o.RetainMode == meta.LockModeGovernance && bypassGovernance {
		return false
	}
	return true
}
