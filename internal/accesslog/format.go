package accesslog

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/danchupin/strata/internal/meta"
)

// FormatLine renders one buffered access-log row as an AWS S3 server-access-log
// line. Field order follows the AWS spec:
//
//	Bucket Owner | Bucket | Time | Remote IP | Requester | Request-ID |
//	Operation | Key | Request-URI | HTTP status | Error Code | Bytes Sent |
//	Object Size | Total Time | Turn-Around Time | Referrer | User-Agent |
//	Version Id
//
// Empty / unknown scalar fields are rendered as "-"; quoted fields ("URI",
// Referrer, User-Agent) are always emitted with quotes (AWS-compatible).
func FormatLine(owner string, e meta.AccessLogEntry) string {
	uri := buildRequestURI(e)
	return strings.Join([]string{
		dashIfEmpty(owner),
		dashIfEmpty(e.Bucket),
		"[" + e.Time.UTC().Format("02/Jan/2006:15:04:05 -0700") + "]",
		dashIfEmpty(e.SourceIP),
		dashIfEmpty(e.Principal),
		dashIfEmpty(e.RequestID),
		dashIfEmpty(e.Op),
		dashIfEmpty(e.Key),
		quote(uri),
		fmt.Sprintf("%d", e.Status),
		"-", // Error Code (not yet plumbed into AccessLogEntry).
		intOrDash(e.BytesSent),
		intOrDash(e.ObjectSize),
		fmt.Sprintf("%d", e.TotalTimeMS),
		fmt.Sprintf("%d", e.TurnAroundMS),
		quote(dashIfEmpty(e.Referrer)),
		quote(dashIfEmpty(e.UserAgent)),
		dashIfEmpty(e.VersionID),
	}, " ")
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func intOrDash(n int64) string {
	if n <= 0 {
		return "-"
	}
	return fmt.Sprintf("%d", n)
}

func quote(s string) string {
	return `"` + s + `"`
}

// buildRequestURI reconstructs the "<METHOD> <PATH> HTTP/1.1" string from the
// stored Op + bucket/key + version. Op shape is "REST.<METHOD>.<RESOURCE>".
func buildRequestURI(e meta.AccessLogEntry) string {
	method := "GET"
	if parts := strings.SplitN(e.Op, ".", 3); len(parts) >= 2 {
		method = parts[1]
	}
	path := "/" + e.Bucket
	if e.Key != "" {
		path += "/" + e.Key
	}
	if e.VersionID != "" {
		path += "?versionId=" + e.VersionID
	}
	return method + " " + path + " HTTP/1.1"
}

// flushFileName builds the per-flush object key under TargetPrefix:
//
//	<TargetPrefix>YYYY-MM-DD-HH-MM-SS-<id>
//
// AWS uses an opaque 16-hex id; we derive it from a deterministic-but-unique
// MD5 of (now, sourceBucket, count) so collisions across overlapping flushes
// stay vanishingly unlikely.
func flushFileName(prefix, sourceBucket string, now time.Time, rowCount int) string {
	stamp := now.UTC().Format("2006-01-02-15-04-05")
	h := md5.Sum(fmt.Appendf(nil, "%s|%s|%d|%d", sourceBucket, stamp, rowCount, now.UnixNano()))
	return prefix + stamp + "-" + strings.ToUpper(hex.EncodeToString(h[:8]))
}
