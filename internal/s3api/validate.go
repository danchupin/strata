package s3api

import (
	"net"
	"strings"
	"unicode/utf8"
)

// validObjectKey rejects keys that AWS rejects with InvalidURI: invalid UTF-8
// or any C0/C1 control codepoint (U+0000..U+001F or U+007F..U+009F).
func validObjectKey(key string) bool {
	if !utf8.ValidString(key) {
		return false
	}
	for _, r := range key {
		if (r >= 0x00 && r <= 0x1f) || (r >= 0x7f && r <= 0x9f) {
			return false
		}
	}
	return true
}

// reservedBucketNames are gateway-internal path prefixes that must not
// collide with bucket names (path-style addressing puts the bucket at
// /<name>/...). Add new entries when registering new top-level routes.
var reservedBucketNames = map[string]struct{}{
	"console":     {}, // /console/* serves the embedded operator UI
	"admin":       {}, // /admin/v1/* JSON API (Phase 1 web UI)
	"metrics":     {}, // /metrics is the Prometheus exposition endpoint
	"healthz":     {}, // /healthz liveness probe
	"readyz":      {}, // /readyz readiness probe
	".well-known": {}, // RFC 5785 reserved
}

// ValidBucketName is the public alias for validBucketName so packages outside
// s3api (e.g. adminapi) can validate names against the same rules without
// duplicating the regex.
func ValidBucketName(name string) bool { return validBucketName(name) }

// ValidCORSBlob runs the s3api parser on a candidate CORS XML blob and
// returns true when the s3api consumer would accept it. Used by adminapi to
// pre-flight operator-supplied configurations before SetBucketCORS persists.
func ValidCORSBlob(blob []byte) bool {
	_, err := parseCORSConfig(blob)
	return err == nil
}

// validBucketName checks the S3 DNS-safe bucket name rules:
//
//	length 3..63, lowercase letters / digits / hyphen / dot,
//	starts and ends with letter or digit, no consecutive dots,
//	not an IP address, no ".-" or "-." joins,
//	not a reserved gateway-internal route name.
func validBucketName(name string) bool {
	if _, reserved := reservedBucketNames[name]; reserved {
		return false
	}
	if len(name) < 3 || len(name) > 63 {
		return false
	}
	first := name[0]
	last := name[len(name)-1]
	if !(isLowerAlphaNum(first) && isLowerAlphaNum(last)) {
		return false
	}
	if net.ParseIP(name) != nil {
		return false
	}
	if strings.Contains(name, "..") || strings.Contains(name, ".-") || strings.Contains(name, "-.") {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case 'a' <= c && c <= 'z':
		case '0' <= c && c <= '9':
		case c == '-' || c == '.':
		default:
			return false
		}
	}
	return true
}

func isLowerAlphaNum(c byte) bool {
	return ('a' <= c && c <= 'z') || ('0' <= c && c <= '9')
}
