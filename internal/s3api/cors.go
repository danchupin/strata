package s3api

import (
	"encoding/xml"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/danchupin/strata/internal/data"
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
	cfg, err := parseCORSConfig(body)
	if err != nil {
		writeError(w, r, ErrMalformedXML)
		return
	}
	if err := s.Meta.SetBucketCORS(r.Context(), b.ID, body); err != nil {
		mapMetaErr(w, r, err)
		return
	}
	// US-015: when the data backend supports bidirectional CORS mapping
	// (s3 backend), mirror the parsed rules onto the backend bucket so
	// preflight OPTIONS against backend-presigned URLs (US-016) hit the
	// same rule set. Strata-stored config remains the source of truth;
	// translation failures are logged at WARN and do NOT fail the user
	// request.
	if cb, ok := s.Data.(data.CORSBackend); ok {
		rules := translateCORSRules(cfg)
		if err := cb.PutBackendCORS(r.Context(), rules); err != nil {
			slog.Warn("s3 cors backend translation: push failed",
				"bucket", bucket, "err", err)
		}
	}
	w.WriteHeader(http.StatusOK)
}

// translateCORSRules flattens a parsed Strata CORSConfiguration into the
// data.CORSRule shape the backend translator consumes. The XML and
// data-layer shapes are field-for-field equivalent — translation is a
// straight copy with defensive slice cloning so the meta-stored rule slice
// and the backend-pushed slice are independent.
func translateCORSRules(cfg *corsConfiguration) []data.CORSRule {
	out := make([]data.CORSRule, 0, len(cfg.Rules))
	for _, r := range cfg.Rules {
		out = append(out, data.CORSRule{
			ID:             r.ID,
			AllowedMethods: append([]string(nil), r.AllowedMethods...),
			AllowedOrigins: append([]string(nil), r.AllowedOrigins...),
			AllowedHeaders: append([]string(nil), r.AllowedHeaders...),
			ExposeHeaders:  append([]string(nil), r.ExposeHeaders...),
			MaxAgeSeconds:  r.MaxAgeSeconds,
		})
	}
	return out
}

func (s *Server) getBucketCORS(w http.ResponseWriter, r *http.Request, bucket string) {
	b, err := s.Meta.GetBucket(r.Context(), bucket)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	blob, metaErr := s.Meta.GetBucketCORS(r.Context(), b.ID)
	hasStrata := metaErr == nil
	if metaErr != nil && !errors.Is(metaErr, meta.ErrNoSuchCORS) {
		mapMetaErr(w, r, metaErr)
		return
	}

	// US-015: union the Strata-stored config with backend-stored rules.
	// Strata takes precedence on conflict (matching ID is dropped from the
	// backend slice). When the backend backend errors or surfaces no rules,
	// fall back to the Strata blob as-is.
	merged, mergedOK := s.maybeMergeCORS(r, blob, hasStrata)
	if mergedOK {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(merged)
		return
	}

	if !hasStrata {
		writeError(w, r, ErrNoSuchCORSConfiguration)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(blob)
}

// maybeMergeCORS unions Strata-stored rules with backend-stored rules.
// Returns the merged XML blob and ok=true when a backend impl is present
// AND produced at least one extra rule (or filled in rules when Strata
// had none). Returns ok=false when no merge is needed — the caller falls
// back to the Strata-stored blob as the wire response.
//
// Conflict resolution: rules are keyed by ID. When Strata and backend
// have a rule with the same non-empty ID, the Strata rule wins (the
// backend's copy is dropped). Empty IDs never collide; backend rules
// with empty IDs are appended verbatim.
func (s *Server) maybeMergeCORS(r *http.Request, strataBlob []byte, hasStrata bool) ([]byte, bool) {
	cb, ok := s.Data.(data.CORSBackend)
	if !ok {
		return nil, false
	}
	backendRules, err := cb.GetBackendCORS(r.Context())
	if err != nil {
		slog.Warn("s3 cors backend translation: get failed; falling back to strata-stored",
			"err", err)
		return nil, false
	}
	if len(backendRules) == 0 {
		return nil, false
	}

	var strataCfg corsConfiguration
	if hasStrata {
		if err := xml.Unmarshal(strataBlob, &strataCfg); err != nil {
			return nil, false
		}
	}
	strataIDs := make(map[string]struct{}, len(strataCfg.Rules))
	for _, r := range strataCfg.Rules {
		if r.ID != "" {
			strataIDs[r.ID] = struct{}{}
		}
	}

	out := strataCfg
	added := false
	for _, br := range backendRules {
		if br.ID != "" {
			if _, dup := strataIDs[br.ID]; dup {
				continue
			}
		}
		out.Rules = append(out.Rules, corsRule{
			ID:             br.ID,
			AllowedMethods: br.AllowedMethods,
			AllowedOrigins: br.AllowedOrigins,
			AllowedHeaders: br.AllowedHeaders,
			ExposeHeaders:  br.ExposeHeaders,
			MaxAgeSeconds:  br.MaxAgeSeconds,
		})
		added = true
	}
	if !hasStrata && !added {
		return nil, false
	}
	if hasStrata && !added {
		// Strata config covers everything backend reported — reuse the
		// stored blob verbatim instead of round-tripping through the
		// XML marshaller (preserves operator's original formatting).
		return nil, false
	}
	body, err := xml.Marshal(out)
	if err != nil {
		return nil, false
	}
	return body, true
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
	// US-015: clear backend CORS too so derived state doesn't outlive the
	// source of truth. Errors are non-fatal — user already saw success at
	// the meta layer; backend cleanup is best-effort.
	if cb, ok := s.Data.(data.CORSBackend); ok {
		if err := cb.DeleteBackendCORS(r.Context()); err != nil {
			slog.Warn("s3 cors backend translation: delete failed",
				"bucket", bucket, "err", err)
		}
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
