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
	NodeCount        int    `json:"node_count"`
	NodeCountHealthy int    `json:"node_count_healthy"`
	MetaBackend      string `json:"meta_backend"`
	DataBackend      string `json:"data_backend"`
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
	Series []MetricSeries `json:"series"`
}

type MetricSeries struct {
	Name   string        `json:"name"`
	Points []MetricPoint `json:"points"`
}

// MetricPoint marshals as [<epoch-ms>, <value>] to match the standard
// Prometheus instant-vector point shape.
type MetricPoint [2]float64
