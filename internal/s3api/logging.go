package s3api

import (
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/danchupin/strata/internal/meta"
)

const emptyBucketLoggingStatus = `<?xml version="1.0" encoding="UTF-8"?>
<BucketLoggingStatus xmlns="http://s3.amazonaws.com/doc/2006-03-01/"></BucketLoggingStatus>`

type bucketLoggingStatus struct {
	XMLName        xml.Name              `xml:"BucketLoggingStatus"`
	LoggingEnabled *loggingEnabledConfig `xml:"LoggingEnabled,omitempty"`
}

type loggingEnabledConfig struct {
	TargetBucket string `xml:"TargetBucket"`
	TargetPrefix string `xml:"TargetPrefix"`
}

func (s *Server) putBucketLogging(w http.ResponseWriter, r *http.Request, bucket string) {
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
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		if err := s.Meta.DeleteBucketLogging(r.Context(), b.ID); err != nil {
			mapMetaErr(w, r, err)
			return
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	var cfg bucketLoggingStatus
	if err := xml.Unmarshal(body, &cfg); err != nil {
		writeError(w, r, ErrMalformedXML)
		return
	}
	if cfg.LoggingEnabled != nil && strings.TrimSpace(cfg.LoggingEnabled.TargetBucket) == "" {
		writeError(w, r, ErrMalformedXML)
		return
	}
	// LoggingEnabled absent → equivalent to clearing.
	if cfg.LoggingEnabled == nil {
		if err := s.Meta.DeleteBucketLogging(r.Context(), b.ID); err != nil {
			mapMetaErr(w, r, err)
			return
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	if err := s.Meta.SetBucketLogging(r.Context(), b.ID, body); err != nil {
		mapMetaErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) getBucketLogging(w http.ResponseWriter, r *http.Request, bucket string) {
	b, err := s.Meta.GetBucket(r.Context(), bucket)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	blob, err := s.Meta.GetBucketLogging(r.Context(), b.ID)
	if err != nil {
		if errors.Is(err, meta.ErrNoSuchLogging) {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(emptyBucketLoggingStatus))
			return
		}
		mapMetaErr(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(blob)
}
