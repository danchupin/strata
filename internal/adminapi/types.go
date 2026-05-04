package adminapi

// Response shapes for /admin/v1/* endpoints. Phase 1 ships stub data; the
// JSON contract is the stable surface — subsequent stories populate fields
// without renaming or removing them.

type ClusterStatus struct {
	Status           string `json:"status"`
	Version          string `json:"version"`
	StartedAt        int64  `json:"started_at"`
	UptimeSec        int64  `json:"uptime_sec"`
	ClusterName      string `json:"cluster_name"`
	Region           string `json:"region"`
	NodeCount        int    `json:"node_count"`
	NodeCountHealthy int    `json:"node_count_healthy"`
	MetaBackend      string `json:"meta_backend"`
	DataBackend      string `json:"data_backend"`
	// OtelEndpoint mirrors OTEL_EXPORTER_OTLP_ENDPOINT. When non-empty the
	// trace browser UI (US-006) renders an "Open in Jaeger" deep link so
	// operators can pivot from the in-process ring buffer to the long-term
	// trace store. Omitted from the response when unset.
	OtelEndpoint string `json:"otel_endpoint,omitempty"`
}

// CreateBucketRequest is the JSON body accepted by POST /admin/v1/buckets.
// Region defaults to the gateway's configured RegionName when empty.
// Versioning accepts "Enabled" or "Suspended" (case-insensitive); empty
// means Suspended. ObjectLockEnabled requires Versioning="Enabled".
type CreateBucketRequest struct {
	Name              string `json:"name"`
	Region            string `json:"region"`
	Versioning        string `json:"versioning"`
	ObjectLockEnabled bool   `json:"object_lock_enabled"`
}

type ClusterNodesResponse struct {
	Nodes []ClusterNode `json:"nodes"`
}

type ClusterNode struct {
	ID            string   `json:"id"`
	Address       string   `json:"address"`
	Version       string   `json:"version"`
	StartedAt     int64    `json:"started_at"`
	UptimeSec     int64    `json:"uptime_sec"`
	Status        string   `json:"status"`
	Workers       []string `json:"workers"`
	LeaderFor     []string `json:"leader_for"`
	LastHeartbeat int64    `json:"last_heartbeat"`
}

type BucketsListResponse struct {
	Buckets []BucketSummary `json:"buckets"`
	Total   int             `json:"total"`
}

type BucketSummary struct {
	Name        string `json:"name"`
	Owner       string `json:"owner"`
	Region      string `json:"region"`
	CreatedAt   int64  `json:"created_at"`
	SizeBytes   int64  `json:"size_bytes"`
	ObjectCount int64  `json:"object_count"`
}

// BucketDetail is the response shape for GET /admin/v1/buckets/{bucket}
// (US-011 bucket detail page). Versioning maps the meta-store enum
// (Disabled|Enabled|Suspended) to the operator-facing label
// (Off|Enabled|Suspended). ObjectLock is always false in Phase 1 — bucket
// object-lock state is not persisted on meta.Bucket today; Phase 2 lifts it.
type BucketDetail struct {
	Name           string `json:"name"`
	Owner          string `json:"owner"`
	Region         string `json:"region"`
	CreatedAt      int64  `json:"created_at"`
	Versioning     string `json:"versioning"`
	ObjectLock     bool   `json:"object_lock"`
	SizeBytes      int64  `json:"size_bytes"`
	ObjectCount    int64  `json:"object_count"`
	BackendPresign bool   `json:"backend_presign"`
}

// SetBackendPresignRequest is the JSON body accepted by PUT /admin/v1/buckets/
// {bucket}/backend-presign (US-020). Flips the per-bucket s3-over-s3 backend
// presign-passthrough flag.
type SetBackendPresignRequest struct {
	Enabled bool `json:"enabled"`
}

type BucketsTopResponse struct {
	Buckets          []BucketTop `json:"buckets"`
	MetricsAvailable bool        `json:"metrics_available"`
}

type BucketTop struct {
	Name            string `json:"name"`
	SizeBytes       int64  `json:"size_bytes"`
	ObjectCount     int64  `json:"object_count"`
	RequestCount24h int64  `json:"request_count_24h"`
}

type ObjectsListResponse struct {
	Objects        []ObjectSummary `json:"objects"`
	CommonPrefixes []string        `json:"common_prefixes"`
	NextMarker     string          `json:"next_marker"`
	IsTruncated    bool            `json:"is_truncated"`
}

type ObjectSummary struct {
	Key          string `json:"key"`
	Size         int64  `json:"size"`
	LastModified int64  `json:"last_modified"`
	ETag         string `json:"etag"`
	StorageClass string `json:"storage_class"`
}

type ConsumersTopResponse struct {
	Consumers        []ConsumerTop `json:"consumers"`
	MetricsAvailable bool          `json:"metrics_available"`
}

type ConsumerTop struct {
	AccessKey       string `json:"access_key"`
	User            string `json:"user"`
	RequestCount24h int64  `json:"request_count_24h"`
	Bytes24h        int64  `json:"bytes_24h"`
}

type MetricsTimeseriesResponse struct {
	Series           []MetricSeries `json:"series"`
	MetricsAvailable bool           `json:"metrics_available"`
}

type MetricSeries struct {
	Name   string        `json:"name"`
	Points []MetricPoint `json:"points"`
}

// MetricPoint marshals as [<epoch-ms>, <value>] to match the standard
// Prometheus instant-vector point shape.
type MetricPoint [2]float64

// SetVersioningRequest is the JSON body accepted by PUT /admin/v1/buckets/
// {bucket}/versioning. State must be "Enabled" or "Suspended"; "Disabled"
// is rejected with 400 (a freshly-created bucket starts at Disabled and
// cannot be flipped back to it via the operator console).
type SetVersioningRequest struct {
	State string `json:"state"`
}

// ObjectLockConfigJSON is the AWS ObjectLockConfiguration shape rendered as
// JSON for /admin/v1/buckets/{bucket}/object-lock. Mirrors the XML form
// served on the S3 surface (PutObjectLockConfiguration). Rule omitted means
// "no default retention". Mode is GOVERNANCE or COMPLIANCE; Days and Years
// are mutually exclusive (server returns 400 InvalidArgument otherwise).
type ObjectLockConfigJSON struct {
	ObjectLockEnabled string                   `json:"object_lock_enabled,omitempty"`
	Rule              *ObjectLockRuleJSON      `json:"rule,omitempty"`
}

type ObjectLockRuleJSON struct {
	DefaultRetention *ObjectLockDefaultRetentionJSON `json:"default_retention,omitempty"`
}

type ObjectLockDefaultRetentionJSON struct {
	Mode  string `json:"mode,omitempty"`
	Days  *int   `json:"days,omitempty"`
	Years *int   `json:"years,omitempty"`
}

// ForceEmptyJobResponse is the JSON shape of POST /admin/v1/buckets/{bucket}
// /force-empty (returns 202 + body) and GET .../force-empty/{jobID}. State
// is one of meta.AdminJobState{Pending,Running,Done,Error}. Deleted is the
// running tally of objects deleted so far. Message carries the last-error
// blurb when State == "error".
type ForceEmptyJobResponse struct {
	JobID      string `json:"job_id"`
	Bucket     string `json:"bucket"`
	State      string `json:"state"`
	Deleted    int64  `json:"deleted"`
	Message    string `json:"message,omitempty"`
	StartedAt  int64  `json:"started_at"`
	UpdatedAt  int64  `json:"updated_at"`
	FinishedAt int64  `json:"finished_at,omitempty"`
}
