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

// validBucketName checks the S3 DNS-safe bucket name rules:
//   length 3..63, lowercase letters / digits / hyphen / dot,
//   starts and ends with letter or digit, no consecutive dots,
//   not an IP address, no ".-" or "-." joins.
func validBucketName(name string) bool {
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
