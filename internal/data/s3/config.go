package s3

import (
	"time"
)

// ProbeKey is the sentinel object used by the boot-time writability
// probe (US-005). Strata writes + deletes this key against the backend
// bucket once on startup; it never appears during steady-state traffic.
const ProbeKey = ".strata-readyz-canary"

const (
	// DefaultPartSize matches PRD US-002: 16 MiB part size keeps the memory
	// bound at PartSize * UploadConcurrency = 64 MiB peak with defaults.
	DefaultPartSize int64 = 16 * 1024 * 1024
	// DefaultUploadConcurrency is the per-Put parallelism for multipart
	// part uploads. PRD US-002 default = 4.
	DefaultUploadConcurrency = 4
	// DefaultMaxRetries matches PRD US-006: max attempts = 5.
	DefaultMaxRetries = 5
	// DefaultOpTimeout matches PRD US-006: 30 s for small ops.
	DefaultOpTimeout = 30 * time.Second
	// DefaultMultipartTimeout matches PRD US-006: 10 min for the whole
	// multipart Put lifecycle (init + parts + complete).
	DefaultMultipartTimeout = 10 * time.Minute
)
