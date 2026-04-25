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

type corsRule struct {
	XMLName        xml.Name `xml:"CORSRule"`
	ID             string   `xml:"ID,omitempty"`
	AllowedMethods []string `xml:"AllowedMethod"`
	AllowedOrigins []string `xml:"AllowedOrigin"`
	AllowedHeaders []string `xml:"AllowedHeader"`
	ExposeHeaders  []string `xml:"ExposeHeader"`
	MaxAgeSeconds  int      `xml:"MaxAgeSeconds,omitempty"`
}

type corsConfiguration struct {
	XMLName xml.Name   `xml:"CORSConfiguration"`
	Rules   []corsRule `xml:"CORSRule"`
}

func parseCORSConfig(blob []byte) (*corsConfiguration, error) {
	var cfg corsConfiguration
	if err := xml.Unmarshal(blob, &cfg); err != nil {
		return nil, err
	}
	if len(cfg.Rules) == 0 {
		return nil, errors.New("no rules")
	}
	for _, r := range cfg.Rules {
		if len(r.AllowedMethods) == 0 || len(r.AllowedOrigins) == 0 {
			return nil, errors.New("rule missing AllowedMethod or AllowedOrigin")
		}
	}
	return &cfg, nil
}

func (s *Server) putBucketCORS(w http.ResponseWriter, r *http.Request, bucket string) {
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
	if _, err := parseCORSConfig(body); err != nil {
		writeError(w, r, ErrMalformedXML)
		return
	}
	if err := s.Meta.SetBucketCORS(r.Context(), b.ID, body); err != nil {
		mapMetaErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) getBucketCORS(w http.ResponseWriter, r *http.Request, bucket string) {
	b, err := s.Meta.GetBucket(r.Context(), bucket)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	blob, err := s.Meta.GetBucketCORS(r.Context(), b.ID)
	if err != nil {
		if errors.Is(err, meta.ErrNoSuchCORS) {
			writeError(w, r, ErrNoSuchCORSConfiguration)
			return
		}
		mapMetaErr(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(blob)
}

func (s *Server) deleteBucketCORS(w http.ResponseWriter, r *http.Request, bucket string) {
	b, err := s.Meta.GetBucket(r.Context(), bucket)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	if err := s.Meta.DeleteBucketCORS(r.Context(), b.ID); err != nil {
		mapMetaErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// corsPreflight handles an OPTIONS request: matches Origin + Access-Control-Request-Method
// against the bucket's CORS rules and writes the matching response headers.
func (s *Server) corsPreflight(w http.ResponseWriter, r *http.Request, bucket string) {
	origin := r.Header.Get("Origin")
	method := r.Header.Get("Access-Control-Request-Method")
	if origin == "" || method == "" {
		writeError(w, r, ErrInvalidArgument)
		return
	}
	b, err := s.Meta.GetBucket(r.Context(), bucket)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	blob, err := s.Meta.GetBucketCORS(r.Context(), b.ID)
	if err != nil {
		writeError(w, r, ErrCORSNotEnabled)
		return
	}
	cfg, err := parseCORSConfig(blob)
	if err != nil {
		writeError(w, r, ErrCORSNotEnabled)
		return
	}
	reqHeaders := splitHeaderList(r.Header.Get("Access-Control-Request-Headers"))
	for _, rule := range cfg.Rules {
		if !matchAny(rule.AllowedMethods, method, false) {
			continue
		}
		if !matchAnyOrigin(rule.AllowedOrigins, origin) {
			continue
		}
		if !headersMatch(rule.AllowedHeaders, reqHeaders) {
			continue
		}
		applyCORSHeaders(w, rule, origin)
		w.WriteHeader(http.StatusOK)
		return
	}
	writeError(w, r, ErrCORSNotEnabled)
}

func applyCORSHeaders(w http.ResponseWriter, rule corsRule, origin string) {
	if matchAny(rule.AllowedOrigins, "*", false) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
	} else {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Add("Vary", "Origin")
	}
	w.Header().Set("Access-Control-Allow-Methods", strings.Join(rule.AllowedMethods, ", "))
	if len(rule.AllowedHeaders) > 0 {
		w.Header().Set("Access-Control-Allow-Headers", strings.Join(rule.AllowedHeaders, ", "))
	}
	if len(rule.ExposeHeaders) > 0 {
		w.Header().Set("Access-Control-Expose-Headers", strings.Join(rule.ExposeHeaders, ", "))
	}
	if rule.MaxAgeSeconds > 0 {
		w.Header().Set("Access-Control-Max-Age", strconv.Itoa(rule.MaxAgeSeconds))
	}
}

func splitHeaderList(v string) []string {
	if v == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		t := strings.TrimSpace(p)
		if t != "" {
			out = append(out, t)
		}
	}
	return out
}

func matchAny(patterns []string, value string, caseInsensitive bool) bool {
	for _, p := range patterns {
		if matchPattern(p, value, caseInsensitive) {
			return true
		}
	}
	return false
}

func matchAnyOrigin(patterns []string, origin string) bool {
	return matchAny(patterns, origin, false)
}

func headersMatch(allowed []string, requested []string) bool {
	if len(requested) == 0 {
		return true
	}
	for _, h := range requested {
		if !matchAny(allowed, h, true) {
			return false
		}
	}
	return true
}

// matchPattern supports a single '*' wildcard anywhere in the pattern.
func matchPattern(pattern, value string, caseInsensitive bool) bool {
	if caseInsensitive {
		pattern = strings.ToLower(pattern)
		value = strings.ToLower(value)
	}
	if pattern == "*" {
		return true
	}
	if !strings.Contains(pattern, "*") {
		return pattern == value
	}
	// Split on '*' and require ordered substring matches.
	parts := strings.Split(pattern, "*")
	pos := 0
	for i, part := range parts {
		if part == "" {
			continue
		}
		idx := strings.Index(value[pos:], part)
		if idx < 0 {
			return false
		}
		if i == 0 && idx != 0 {
			return false
		}
		pos += idx + len(part)
	}
	if !strings.HasSuffix(pattern, "*") && pos != len(value) {
		return false
	}
	return true
}
