package s3api

import (
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/danchupin/strata/internal/meta"
)

// objectLockConfiguration mirrors the AWS PutObjectLockConfiguration body.
type objectLockConfiguration struct {
	XMLName           xml.Name `xml:"ObjectLockConfiguration"`
	ObjectLockEnabled string   `xml:"ObjectLockEnabled,omitempty"`
	Rule              *struct {
		DefaultRetention *struct {
			Mode  string `xml:"Mode,omitempty"`
			Days  *int   `xml:"Days,omitempty"`
			Years *int   `xml:"Years,omitempty"`
		} `xml:"DefaultRetention,omitempty"`
	} `xml:"Rule,omitempty"`
}

func (s *Server) putBucketObjectLockConfig(w http.ResponseWriter, r *http.Request, bucket string) {
	b, err := s.Meta.GetBucket(r.Context(), bucket)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	if !b.ObjectLockEnabled {
		writeError(w, r, ErrObjectLockNotEnabled)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil || len(body) == 0 {
		writeError(w, r, ErrMalformedXML)
		return
	}
	var cfg objectLockConfiguration
	if err := xml.Unmarshal(body, &cfg); err != nil {
		writeError(w, r, ErrMalformedXML)
		return
	}
	if cfg.Rule != nil && cfg.Rule.DefaultRetention != nil {
		dr := cfg.Rule.DefaultRetention
		if dr.Mode != "" && dr.Mode != meta.LockModeGovernance && dr.Mode != meta.LockModeCompliance {
			writeError(w, r, ErrMalformedXML)
			return
		}
		if dr.Days != nil && *dr.Days <= 0 {
			writeError(w, r, ErrInvalidArgument)
			return
		}
		if dr.Years != nil && *dr.Years <= 0 {
			writeError(w, r, ErrInvalidArgument)
			return
		}
		if dr.Days != nil && dr.Years != nil {
			writeError(w, r, ErrMalformedXML)
			return
		}
	}
	if err := s.Meta.SetBucketObjectLockConfig(r.Context(), b.ID, body); err != nil {
		mapMetaErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) getBucketObjectLockConfig(w http.ResponseWriter, r *http.Request, bucket string) {
	b, err := s.Meta.GetBucket(r.Context(), bucket)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	blob, err := s.Meta.GetBucketObjectLockConfig(r.Context(), b.ID)
	if err != nil {
		if errors.Is(err, meta.ErrNoSuchObjectLockConfig) {
			writeError(w, r, ErrNoSuchObjectLockConfig)
			return
		}
		mapMetaErr(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(blob)
}

// resolveDefaultRetention returns (mode, retainUntil) drawn from the bucket's
// persisted ObjectLockConfiguration default rule. Zero values are returned when
// no default is configured. Caller skips this when the request already provided
// per-object lock headers.
func (s *Server) resolveDefaultRetention(r *http.Request, b *meta.Bucket) (string, time.Time) {
	if !b.ObjectLockEnabled {
		return "", time.Time{}
	}
	blob, err := s.Meta.GetBucketObjectLockConfig(r.Context(), b.ID)
	if err != nil {
		return "", time.Time{}
	}
	var cfg objectLockConfiguration
	if err := xml.Unmarshal(blob, &cfg); err != nil {
		return "", time.Time{}
	}
	if cfg.Rule == nil || cfg.Rule.DefaultRetention == nil {
		return "", time.Time{}
	}
	dr := cfg.Rule.DefaultRetention
	if dr.Mode == "" {
		return "", time.Time{}
	}
	now := time.Now().UTC()
	switch {
	case dr.Days != nil && *dr.Days > 0:
		return dr.Mode, now.AddDate(0, 0, *dr.Days)
	case dr.Years != nil && *dr.Years > 0:
		return dr.Mode, now.AddDate(*dr.Years, 0, 0)
	}
	return "", time.Time{}
}

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
