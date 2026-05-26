// Package trustedproxies parses STRATA_TRUSTED_PROXIES (CIDR list) and
// exposes the trust-policy primitive consumed by every forwarded-header
// call site in s3api + adminapi. Standalone package to avoid the import
// cycle s3api → serverapp (serverapp already imports s3api). US-007
// harden-gateway.
package trustedproxies

import (
	"fmt"
	"net"
	"net/http"
	"strings"
)

// TrustedProxies carries the parsed CIDR list. Methods are safe to call on
// a nil receiver — empty list = forwarded headers NEVER trusted (production
// default for direct-exposed gateways).
type TrustedProxies struct {
	nets []*net.IPNet
}

// Parse reads a comma-separated CIDR list (IPv4 or IPv6). Empty input
// returns an empty (but non-nil) TrustedProxies. Any malformed entry fails
// fast with the offending substring quoted.
func Parse(s string) (*TrustedProxies, error) {
	tp := &TrustedProxies{}
	s = strings.TrimSpace(s)
	if s == "" {
		return tp, nil
	}
	for raw := range strings.SplitSeq(s, ",") {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		_, ipNet, err := net.ParseCIDR(entry)
		if err != nil {
			return nil, fmt.Errorf("trusted_proxies: %q: %w", entry, err)
		}
		tp.nets = append(tp.nets, ipNet)
	}
	return tp, nil
}

// Empty reports whether the trust list contains no CIDRs.
func (t *TrustedProxies) Empty() bool {
	if t == nil {
		return true
	}
	return len(t.nets) == 0
}

// Contains reports whether remoteAddr (`host:port` or bare IP) falls inside
// any configured CIDR. A nil receiver / empty list returns false so the
// secure default is "never trust".
func (t *TrustedProxies) Contains(remoteAddr string) bool {
	if t == nil || len(t.nets) == 0 {
		return false
	}
	ip := parseIP(remoteAddr)
	if ip == nil {
		return false
	}
	for _, n := range t.nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// ClientIP returns the original client IP per RFC 7239 left-to-right
// discipline when r.RemoteAddr falls inside a trusted CIDR; otherwise the
// bare host portion of r.RemoteAddr (untrusted source — forwarded headers
// ignored). Headers consulted in order: X-Forwarded-For, X-Real-IP.
func (t *TrustedProxies) ClientIP(r *http.Request) string {
	host := remoteHost(r.RemoteAddr)
	if !t.Contains(r.RemoteAddr) {
		return host
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// Left-to-right: pick the first hop that is NOT itself a trusted
		// proxy. Per RFC 7239 §5.2 the leftmost untrusted hop is the
		// original client. Single-value (no comma) is the trivial case.
		for part := range strings.SplitSeq(xff, ",") {
			candidate := strings.TrimSpace(part)
			if candidate == "" {
				continue
			}
			if !t.Contains(candidate) {
				return candidate
			}
		}
	}
	if xri := strings.TrimSpace(r.Header.Get("X-Real-IP")); xri != "" {
		return xri
	}
	return host
}

// ForwardedProto returns "https" when the trusted-source request carries
// `X-Forwarded-Proto: https`, "http" when it carries `http`, or "" when
// the source is untrusted / the header is absent.
func (t *TrustedProxies) ForwardedProto(r *http.Request) string {
	if !t.Contains(r.RemoteAddr) {
		return ""
	}
	v := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")))
	switch v {
	case "http", "https":
		return v
	}
	return ""
}

func parseIP(addr string) net.IP {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return nil
	}
	if host, _, err := net.SplitHostPort(addr); err == nil {
		addr = host
	}
	return net.ParseIP(addr)
}

func remoteHost(remoteAddr string) string {
	if remoteAddr == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return host
	}
	return remoteAddr
}
