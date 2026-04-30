package s3

// Config carries the wiring needed to talk to a single S3-compatible
// backend bucket. US-005 will populate it from STRATA_S3_BACKEND_* env
// vars via koanf; until then callers (tests, US-002 streaming PUT) build
// it directly.
type Config struct {
	// Endpoint is the full backend URL (http://host:port). Empty falls back
	// to the SDK's region-based AWS endpoint resolution.
	Endpoint string
	// Region is required (SDK validation).
	Region string
	// Bucket is the single backend bucket every Strata object lands in.
	Bucket string
	// AccessKey/SecretKey set static creds. Both empty falls through to the
	// SDK default credential chain (env / ~/.aws / IRSA / IMDS).
	AccessKey string
	SecretKey string
	// ForcePathStyle = true is required for MinIO + Ceph RGW; false for AWS.
	ForcePathStyle bool

	// PartSize is the multipart-upload part size in bytes. Zero ⇒
	// DefaultPartSize. Minimum honoured by the SDK is 5 MiB.
	PartSize int64
	// UploadConcurrency is the number of parallel part uploads per Put.
	// Zero ⇒ DefaultUploadConcurrency.
	UploadConcurrency int
}

const (
	// DefaultPartSize matches PRD US-002: 16 MiB part size keeps the memory
	// bound at PartSize * UploadConcurrency = 64 MiB peak with defaults.
	DefaultPartSize int64 = 16 * 1024 * 1024
	// DefaultUploadConcurrency is the per-Put parallelism for multipart
	// part uploads. PRD US-002 default = 4.
	DefaultUploadConcurrency = 4
)
