package s3api

import (
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/danchupin/strata/internal/meta"
)

type websiteConfiguration struct {
	XMLName               xml.Name             `xml:"WebsiteConfiguration"`
	IndexDocument         *websiteIndexDoc     `xml:"IndexDocument,omitempty"`
	ErrorDocument         *websiteErrorDoc     `xml:"ErrorDocument,omitempty"`
	RedirectAllRequestsTo *websiteRedirectAll  `xml:"RedirectAllRequestsTo,omitempty"`
	RoutingRules          *websiteRoutingRules `xml:"RoutingRules,omitempty"`
}

type websiteIndexDoc struct {
	Suffix string `xml:"Suffix"`
}

type websiteErrorDoc struct {
	Key string `xml:"Key"`
}

type websiteRedirectAll struct {
	HostName string `xml:"HostName"`
	Protocol string `xml:"Protocol,omitempty"`
}

type websiteRoutingRules struct {
	Inner []byte `xml:",innerxml"`
}

func (s *Server) putBucketWebsite(w http.ResponseWriter, r *http.Request, bucket string) {
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
	if len(body) == 0 {
		writeError(w, r, ErrMalformedXML)
		return
	}
	var cfg websiteConfiguration
	if err := xml.Unmarshal(body, &cfg); err != nil {
		writeError(w, r, ErrMalformedXML)
		return
	}
	if cfg.RedirectAllRequestsTo == nil {
		if cfg.IndexDocument == nil || strings.TrimSpace(cfg.IndexDocument.Suffix) == "" {
			writeError(w, r, ErrInvalidArgument)
			return
		}
		if strings.Contains(cfg.IndexDocument.Suffix, "/") {
			writeError(w, r, ErrInvalidArgument)
			return
		}
	}
	if err := s.Meta.SetBucketWebsite(r.Context(), b.ID, body); err != nil {
		mapMetaErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) getBucketWebsite(w http.ResponseWriter, r *http.Request, bucket string) {
	b, err := s.Meta.GetBucket(r.Context(), bucket)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	blob, err := s.Meta.GetBucketWebsite(r.Context(), b.ID)
	if err != nil {
		if errors.Is(err, meta.ErrNoSuchWebsite) {
			writeError(w, r, ErrNoSuchWebsiteConfig)
			return
		}
		mapMetaErr(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(blob)
}

func (s *Server) deleteBucketWebsite(w http.ResponseWriter, r *http.Request, bucket string) {
	b, err := s.Meta.GetBucket(r.Context(), bucket)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	if err := s.Meta.DeleteBucketWebsite(r.Context(), b.ID); err != nil {
		mapMetaErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// loadWebsiteConfig returns the parsed website configuration for a bucket, or
// (nil, nil) when no configuration is set. Other errors propagate.
func (s *Server) loadWebsiteConfig(r *http.Request, b *meta.Bucket) (*websiteConfiguration, error) {
	blob, err := s.Meta.GetBucketWebsite(r.Context(), b.ID)
	if err != nil {
		if errors.Is(err, meta.ErrNoSuchWebsite) {
			return nil, nil
		}
		return nil, err
	}
	cfg := &websiteConfiguration{}
	if err := xml.Unmarshal(blob, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// serveWebsiteRoot serves GET /<bucket>/ when website config is present:
// fetches the configured IndexDocument; on miss falls back to ErrorDocument
// with a 404 status. Returns true if the request was handled.
func (s *Server) serveWebsiteRoot(w http.ResponseWriter, r *http.Request, b *meta.Bucket) bool {
	cfg, err := s.loadWebsiteConfig(r, b)
	if err != nil {
		writeError(w, r, ErrInternal)
		return true
	}
	if cfg == nil {
		return false
	}
	if writeWebsiteRedirectAll(w, cfg, "") {
		return true
	}
	if cfg.IndexDocument != nil && cfg.IndexDocument.Suffix != "" {
		if obj, err := s.Meta.GetObject(r.Context(), b.ID, cfg.IndexDocument.Suffix, ""); err == nil && !obj.IsDeleteMarker {
			s.serveWebsiteObject(w, r, obj, http.StatusOK)
			return true
		}
	}
	s.serveWebsiteError(w, r, b, cfg)
	return true
}

// tryWebsiteRedirectAll loads bucket website config and, when
// RedirectAllRequestsTo is set, writes a 301 redirect for the given key
// (empty key for bucket root). Returns true if the request was handled.
func (s *Server) tryWebsiteRedirectAll(w http.ResponseWriter, r *http.Request, b *meta.Bucket, key string) bool {
	cfg, err := s.loadWebsiteConfig(r, b)
	if err != nil || cfg == nil {
		return false
	}
	return writeWebsiteRedirectAll(w, cfg, key)
}

// writeWebsiteRedirectAll emits a 301 Location response when cfg has a
// RedirectAllRequestsTo block. key is appended after a leading slash when
// non-empty. Returns false (no header written) when redirect is not configured.
func writeWebsiteRedirectAll(w http.ResponseWriter, cfg *websiteConfiguration, key string) bool {
	if cfg == nil || cfg.RedirectAllRequestsTo == nil {
		return false
	}
	host := strings.TrimSpace(cfg.RedirectAllRequestsTo.HostName)
	if host == "" {
		return false
	}
	proto := strings.ToLower(strings.TrimSpace(cfg.RedirectAllRequestsTo.Protocol))
	if proto != "http" && proto != "https" {
		proto = "http"
	}
	loc := proto + "://" + host
	if key != "" {
		loc += "/" + key
	}
	w.Header().Set("Location", loc)
	w.WriteHeader(http.StatusMovedPermanently)
	return true
}

func (s *Server) serveWebsiteError(w http.ResponseWriter, r *http.Request, b *meta.Bucket, cfg *websiteConfiguration) {
	if cfg != nil && cfg.ErrorDocument != nil && cfg.ErrorDocument.Key != "" {
		if obj, err := s.Meta.GetObject(r.Context(), b.ID, cfg.ErrorDocument.Key, ""); err == nil && !obj.IsDeleteMarker {
			s.serveWebsiteObject(w, r, obj, http.StatusNotFound)
			return
		}
	}
	writeError(w, r, ErrNoSuchKey)
}

func (s *Server) serveWebsiteObject(w http.ResponseWriter, r *http.Request, o *meta.Object, status int) {
	w.Header().Set("Content-Type", firstNonEmpty(o.ContentType, "application/octet-stream"))
	w.Header().Set("ETag", `"`+o.ETag+`"`)
	w.Header().Set("Last-Modified", o.Mtime.UTC().Format(http.TimeFormat))
	w.Header().Set("Content-Length", strconv.FormatInt(o.Size, 10))
	rc, err := s.Data.GetChunks(r.Context(), o.Manifest, 0, o.Size)
	if err != nil {
		writeError(w, r, ErrInternal)
		return
	}
	defer rc.Close()
	w.WriteHeader(status)
	_, _ = io.Copy(w, rc)
}
