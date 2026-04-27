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
	Rules []websiteRoutingRule `xml:"RoutingRule"`
}

type websiteRoutingRule struct {
	Condition *websiteRoutingCondition `xml:"Condition,omitempty"`
	Redirect  websiteRoutingRedirect   `xml:"Redirect"`
}

type websiteRoutingCondition struct {
	KeyPrefixEquals             string `xml:"KeyPrefixEquals,omitempty"`
	HttpErrorCodeReturnedEquals string `xml:"HttpErrorCodeReturnedEquals,omitempty"`
}

type websiteRoutingRedirect struct {
	Protocol             string  `xml:"Protocol,omitempty"`
	HostName             string  `xml:"HostName,omitempty"`
	ReplaceKeyPrefixWith *string `xml:"ReplaceKeyPrefixWith,omitempty"`
	ReplaceKeyWith       *string `xml:"ReplaceKeyWith,omitempty"`
	HttpRedirectCode     string  `xml:"HttpRedirectCode,omitempty"`
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
	if cfg.RoutingRules != nil {
		if len(cfg.RoutingRules.Rules) == 0 {
			writeError(w, r, ErrInvalidArgument)
			return
		}
		for _, rule := range cfg.RoutingRules.Rules {
			if !validRoutingRule(rule) {
				writeError(w, r, ErrInvalidArgument)
				return
			}
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
	if cfg.RoutingRules != nil {
		for _, rule := range cfg.RoutingRules.Rules {
			if rule.Condition != nil && rule.Condition.HttpErrorCodeReturnedEquals != "" {
				continue
			}
			if newKey, ok := matchRoutingRule(rule, "", 0); ok {
				writeWebsiteRedirect(w, r, rule.Redirect, newKey)
				return true
			}
		}
	}
	if cfg.IndexDocument != nil && cfg.IndexDocument.Suffix != "" {
		if obj, err := s.Meta.GetObject(r.Context(), b.ID, cfg.IndexDocument.Suffix, ""); err == nil && !obj.IsDeleteMarker {
			s.serveWebsiteObject(w, r, obj, http.StatusOK)
			return true
		}
	}
	if cfg.RoutingRules != nil {
		indexKey := ""
		if cfg.IndexDocument != nil {
			indexKey = cfg.IndexDocument.Suffix
		}
		for _, rule := range cfg.RoutingRules.Rules {
			if rule.Condition == nil || rule.Condition.HttpErrorCodeReturnedEquals == "" {
				continue
			}
			if newKey, ok := matchRoutingRule(rule, indexKey, 404); ok {
				writeWebsiteRedirect(w, r, rule.Redirect, newKey)
				return true
			}
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

// tryWebsiteRouting evaluates RoutingRules for an object GET. KeyPrefixEquals
// rules match before the upstream lookup; HttpErrorCodeReturnedEquals rules
// match only when the upstream lookup would 404. Returns true when a redirect
// was written.
func (s *Server) tryWebsiteRouting(w http.ResponseWriter, r *http.Request, b *meta.Bucket, key string) bool {
	cfg, err := s.loadWebsiteConfig(r, b)
	if err != nil || cfg == nil || cfg.RoutingRules == nil || len(cfg.RoutingRules.Rules) == 0 {
		return false
	}
	for _, rule := range cfg.RoutingRules.Rules {
		if rule.Condition != nil && rule.Condition.HttpErrorCodeReturnedEquals != "" {
			continue
		}
		if newKey, ok := matchRoutingRule(rule, key, 0); ok {
			writeWebsiteRedirect(w, r, rule.Redirect, newKey)
			return true
		}
	}
	needErr := false
	for _, rule := range cfg.RoutingRules.Rules {
		if rule.Condition != nil && rule.Condition.HttpErrorCodeReturnedEquals != "" {
			needErr = true
			break
		}
	}
	if !needErr {
		return false
	}
	obj, err := s.Meta.GetObject(r.Context(), b.ID, key, "")
	if err == nil && !obj.IsDeleteMarker {
		return false
	}
	for _, rule := range cfg.RoutingRules.Rules {
		if rule.Condition == nil || rule.Condition.HttpErrorCodeReturnedEquals == "" {
			continue
		}
		if newKey, ok := matchRoutingRule(rule, key, 404); ok {
			writeWebsiteRedirect(w, r, rule.Redirect, newKey)
			return true
		}
	}
	return false
}

// matchRoutingRule returns the rewritten key when the rule's Condition matches
// the given key + errCode (errCode=0 means pre-upstream-lookup; only non-error
// rules match). The rewrite uses ReplaceKeyWith verbatim, or strips
// KeyPrefixEquals + prepends ReplaceKeyPrefixWith, otherwise returns the
// original key.
func matchRoutingRule(rule websiteRoutingRule, key string, errCode int) (string, bool) {
	cond := rule.Condition
	hasPrefix := cond != nil && cond.KeyPrefixEquals != ""
	hasErr := cond != nil && cond.HttpErrorCodeReturnedEquals != ""
	if hasPrefix && !strings.HasPrefix(key, cond.KeyPrefixEquals) {
		return "", false
	}
	if hasErr {
		want, err := strconv.Atoi(cond.HttpErrorCodeReturnedEquals)
		if err != nil || want != errCode {
			return "", false
		}
	}
	newKey := key
	if rule.Redirect.ReplaceKeyWith != nil {
		newKey = *rule.Redirect.ReplaceKeyWith
	} else if rule.Redirect.ReplaceKeyPrefixWith != nil {
		prefix := ""
		if hasPrefix {
			prefix = cond.KeyPrefixEquals
		}
		newKey = *rule.Redirect.ReplaceKeyPrefixWith + strings.TrimPrefix(key, prefix)
	}
	return newKey, true
}

func writeWebsiteRedirect(w http.ResponseWriter, r *http.Request, redirect websiteRoutingRedirect, newKey string) {
	proto := strings.ToLower(strings.TrimSpace(redirect.Protocol))
	if proto != "http" && proto != "https" {
		if r.TLS != nil {
			proto = "https"
		} else {
			proto = "http"
		}
	}
	host := strings.TrimSpace(redirect.HostName)
	if host == "" {
		host = r.Host
	}
	loc := proto + "://" + host
	if newKey != "" {
		loc += "/" + newKey
	}
	status := http.StatusMovedPermanently
	if redirect.HttpRedirectCode != "" {
		if c, err := strconv.Atoi(redirect.HttpRedirectCode); err == nil && c >= 300 && c < 400 {
			status = c
		}
	}
	w.Header().Set("Location", loc)
	w.WriteHeader(status)
}

func validRoutingRule(rule websiteRoutingRule) bool {
	red := rule.Redirect
	if red.HostName == "" && red.ReplaceKeyPrefixWith == nil && red.ReplaceKeyWith == nil {
		return false
	}
	if red.ReplaceKeyPrefixWith != nil && red.ReplaceKeyWith != nil {
		return false
	}
	if red.HttpRedirectCode != "" {
		c, err := strconv.Atoi(red.HttpRedirectCode)
		if err != nil || c < 300 || c >= 400 {
			return false
		}
	}
	if rule.Condition != nil && rule.Condition.HttpErrorCodeReturnedEquals != "" {
		c, err := strconv.Atoi(rule.Condition.HttpErrorCodeReturnedEquals)
		if err != nil || c < 400 || c >= 600 {
			return false
		}
	}
	return true
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
