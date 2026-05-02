package s3api

import (
	"net"
	"strings"
)

// extractVHostBucket returns the bucket prefix when the request Host matches
// any of the configured virtual-hosted-style patterns. Patterns must be of
// the form "*.<suffix>" (e.g. "*.s3.local"); first match wins. The empty
// string is returned when no pattern matches, signalling the caller should
// keep path-style routing.
func extractVHostBucket(host string, patterns []string) string {
	if host == "" || len(patterns) == 0 {
		return ""
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" {
		return ""
	}
	for _, p := range patterns {
		p = strings.TrimSpace(strings.ToLower(p))
		if !strings.HasPrefix(p, "*.") {
			continue
		}
		suffix := p[1:]
		if !strings.HasSuffix(host, suffix) {
			continue
		}
		bucket := host[:len(host)-len(suffix)]
		if bucket == "" {
			continue
		}
		return bucket
	}
	return ""
}

// ParseVHostPatterns splits the comma-separated env value into a slice with
// whitespace trimmed and empty entries dropped.
func ParseVHostPatterns(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := parts[:0]
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
