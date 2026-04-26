package s3api

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"strings"
	"time"
)

// ParseMFASecrets parses an STRATA_MFA_SECRETS-style declaration into a
// map of serial → base32-decoded TOTP secret. The format is comma-separated
// "<serial>:<base32-secret>" pairs (whitespace around entries is trimmed).
// Empty input yields an empty map.
func ParseMFASecrets(raw string) (map[string][]byte, error) {
	out := make(map[string][]byte)
	if strings.TrimSpace(raw) == "" {
		return out, nil
	}
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		idx := strings.LastIndex(entry, ":")
		if idx < 0 {
			return nil, fmt.Errorf("STRATA_MFA_SECRETS entry %q missing ':' separator", entry)
		}
		serial, secret := entry[:idx], entry[idx+1:]
		serial = strings.TrimSpace(serial)
		secret = strings.TrimSpace(secret)
		if serial == "" || secret == "" {
			return nil, fmt.Errorf("STRATA_MFA_SECRETS entry %q has empty serial or secret", entry)
		}
		key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(strings.ReplaceAll(secret, "=", "")))
		if err != nil {
			return nil, fmt.Errorf("STRATA_MFA_SECRETS entry %q: invalid base32 secret: %w", serial, err)
		}
		out[serial] = key
	}
	return out, nil
}

// TOTPForTest exposes the package-internal TOTP function so test fixtures
// can compute the expected code for a fixed clock.
func TOTPForTest(key []byte, t time.Time) string { return totpAt(key, t) }

// totpAt computes the RFC 6238 6-digit TOTP for the given key and time using
// HMAC-SHA1 with a 30-second period.
func totpAt(key []byte, t time.Time) string {
	counter := uint64(t.Unix() / 30)
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], counter)
	mac := hmac.New(sha1.New, key)
	mac.Write(buf[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	code := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", code%1_000_000)
}

// validateMFAHeader returns true when the x-amz-mfa header value
// "<serial> <code>" matches a TOTP for the configured serial's secret in
// the current ±1 window. Missing/malformed headers and unknown serials
// return false.
func (s *Server) validateMFAHeader(headerValue string) bool {
	if headerValue == "" || len(s.MFASecrets) == 0 {
		return false
	}
	serial, code, ok := strings.Cut(strings.TrimSpace(headerValue), " ")
	if !ok {
		return false
	}
	serial = strings.TrimSpace(serial)
	code = strings.TrimSpace(code)
	if serial == "" || code == "" {
		return false
	}
	secret, ok := s.MFASecrets[serial]
	if !ok {
		return false
	}
	now := time.Now
	if s.MFAClock != nil {
		now = s.MFAClock
	}
	t := now()
	for _, drift := range []time.Duration{0, -30 * time.Second, 30 * time.Second} {
		if hmac.Equal([]byte(totpAt(secret, t.Add(drift))), []byte(code)) {
			return true
		}
	}
	return false
}
