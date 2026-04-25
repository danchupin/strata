package s3api

import (
	"net"
	"strings"
)

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
