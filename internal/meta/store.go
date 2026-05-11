package meta

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/data"
)

const (
	VersioningDisabled  = "Disabled"
	VersioningEnabled   = "Enabled"
	VersioningSuspended = "Suspended"
)

// NullVersionID is the sentinel TimeUUID reserved for rows written while a
// bucket's Versioning state is Disabled (or for the single "null" version a
// Suspended bucket retains). Stored in objects.version_id and paired with
// objects.is_null=true. Callers address this row from the wire by the literal
// version-id string "null" (NullVersionLiteral) — both backends translate the
// literal into the sentinel before scanning.
const NullVersionID = "00000000-0000-0000-0000-000000000000"

// NullVersionLiteral is the wire form S3 clients use to address the null
// version (e.g. GET /<key>?versionId=null). Both backends accept it as an
// alias for NullVersionID.
const NullVersionLiteral = "null"

// ResolveVersionID maps the wire form to the stored UUID. "null" → sentinel,
// every other value passes through verbatim. Use this at the entry of any
// meta.Store method that takes a versionID string.
func ResolveVersionID(v string) string {
	if v == NullVersionLiteral {
		return NullVersionID
	}
	return v
}

var (
	ErrBucketNotFound        = errors.New("bucket not found")
	ErrBucketAlreadyExists   = errors.New("bucket already exists")
	ErrBucketNotEmpty        = errors.New("bucket not empty")
	ErrObjectNotFound        = errors.New("object not found")
	ErrObjectLocked          = errors.New("object is protected by retention or legal hold")
	ErrMultipartNotFound     = errors.New("multipart upload not found")
	ErrMultipartInProgress   = errors.New("multipart upload is already completing or aborted")
	ErrMultipartPartMissing  = errors.New("multipart part not found")
	ErrMultipartETagMismatch = errors.New("multipart part etag mismatch")
	ErrNoSuchLifecycle       = errors.New("no lifecycle configuration for bucket")
	ErrNoSuchCORS            = errors.New("no cors configuration for bucket")
	ErrNoSuchBucketPolicy    = errors.New("no policy configured for bucket")
	ErrNoSuchPublicAccessBlock = errors.New("no public access block configuration for bucket")
	ErrNoSuchOwnershipControls = errors.New("no ownership controls configured for bucket")
	ErrNoSuchEncryption        = errors.New("no encryption configuration for bucket")
	ErrNoSuchObjectLockConfig  = errors.New("no object lock configuration for bucket")
	ErrNoSuchNotification      = errors.New("no notification configuration for bucket")
	ErrNoSuchWebsite           = errors.New("no website configuration for bucket")
	ErrNoSuchReplication       = errors.New("no replication configuration for bucket")
	ErrNoSuchLogging           = errors.New("no logging configuration for bucket")
	ErrNoSuchTagSet            = errors.New("no tag set configured for bucket")
	ErrNoSuchGrants            = errors.New("no acl grants persisted for resource")
	ErrNoSuchInventoryConfig   = errors.New("no inventory configuration with that id")
	ErrAccessPointAlreadyExists = errors.New("access point with that name already exists")
	ErrAccessPointNotFound      = errors.New("access point not found")
	ErrReshardInProgress       = errors.New("a reshard is already in progress for this bucket")
	ErrReshardNotFound         = errors.New("no reshard job for this bucket")
	ErrReshardInvalidTarget    = errors.New("reshard target must be a positive power of two greater than the current shard count")
	ErrIAMUserNotFound         = errors.New("iam user not found")
	ErrIAMUserAlreadyExists    = errors.New("iam user already exists")
	ErrIAMAccessKeyNotFound    = errors.New("iam access key not found")
	ErrManagedPolicyNotFound      = errors.New("managed policy not found")
	ErrManagedPolicyAlreadyExists = errors.New("managed policy already exists")
	ErrPolicyAttached             = errors.New("managed policy is attached to one or more users")
	ErrUserPolicyNotAttached      = errors.New("managed policy is not attached to user")
	ErrUserPolicyAlreadyAttached  = errors.New("managed policy is already attached to user")
	ErrMultipartCompletionNotFound = errors.New("multipart completion record not found or expired")
	ErrNoRewrapProgress        = errors.New("no rewrap progress recorded for bucket")
	ErrAdminJobNotFound        = errors.New("admin job not found")
	ErrAdminJobAlreadyExists   = errors.New("admin job already exists")
	// ErrQuotaExceeded signals that a write would exceed a configured
	// per-bucket or per-user quota (US-006). Surfaced from the gateway as
	// HTTP 403 / S3 code "QuotaExceeded" — non-AWS but matches the RGW shape
	// so existing tooling that already understands the code keeps working.
	// Returned today only by gateway-level enforcement helpers; backend
	// stores never raise it directly.
	ErrQuotaExceeded           = errors.New("quota exceeded")
	// ErrInvalidPlacement signals a bucket Placement policy that fails
	// structural validation: weight outside [0, 100], sum(weights) == 0, or
	// an empty cluster name (US-001 placement-rebalance cycle).
	ErrInvalidPlacement        = errors.New("invalid placement policy")
	// ErrUnknownCluster signals a Placement entry references a cluster id
	// that is not in the configured STRATA_RADOS_CLUSTERS / STRATA_S3_CLUSTERS
	// set. The meta.Store itself never raises this — cluster name resolution
	// happens in the admin handler. Defined here so all placement-related
	// sentinels live in one place (US-001).
	ErrUnknownCluster          = errors.New("unknown cluster id")
)

// ValidatePlacement enforces the structural rules on a bucket Placement
// policy: weights in [0, 100], at least one positive weight, no empty
// cluster ids. Cluster-name resolution against the data backend env lives
// in the admin handler (US-001 placement-rebalance cycle).
func ValidatePlacement(p map[string]int) error {
	if len(p) == 0 {
		return ErrInvalidPlacement
	}
	total := 0
	for cluster, w := range p {
		if cluster == "" {
			return ErrInvalidPlacement
		}
		if w < 0 || w > 100 {
			return ErrInvalidPlacement
		}
		total += w
	}
	if total == 0 {
		return ErrInvalidPlacement
	}
	return nil
}

// AdminJob tracks a long-running operator-facing background job kicked off by
// the embedded console (US-002). Today only kind="force-empty" is used: the
// force-empty handler creates an AdminJob, kicks a goroutine that drains the
// bucket via paginated ListObjects + DeleteObject, and updates the row with
// Deleted/State/Message as it progresses. Polled via the
// /admin/v1/buckets/{bucket}/force-empty/{jobID} endpoint.
//
// State transitions: pending -> running -> done | error. Once a job leaves
// pending, only Deleted, UpdatedAt, FinishedAt, State and Message change.
type AdminJob struct {
	ID         string
	Kind       string
	Bucket     string
	State      string
	Message    string
	Deleted    int64
	StartedAt  time.Time
	UpdatedAt  time.Time
	FinishedAt time.Time
}

const (
	AdminJobKindForceEmpty = "force-empty"

	AdminJobStatePending = "pending"
	AdminJobStateRunning = "running"
	AdminJobStateDone    = "done"
	AdminJobStateError   = "error"
)

// RewrapProgress tracks a master-key rewrap pass for a single bucket. Used by
// `strata-admin rewrap` for resumability across runs.
type RewrapProgress struct {
	BucketID  uuid.UUID
	TargetID  string
	LastKey   string
	Complete  bool
	UpdatedAt time.Time
}

// MultipartCompletion is a short-lived idempotency record persisted after a
// successful CompleteMultipartUpload so a retried Complete with the same
// uploadID can replay the original response instead of returning NoSuchUpload.
type MultipartCompletion struct {
	BucketID    uuid.UUID
	UploadID    string
	Key         string
	ETag        string
	VersionID   string
	Body        []byte
	Headers     map[string]string
	CompletedAt time.Time
}

// IAMAccessKey is a credential record minted by IAM CreateAccessKey. The access
// key is the lookup primary key; UserName is a foreign key into IAMUser.
type IAMAccessKey struct {
	AccessKeyID     string
	SecretAccessKey string
	UserName        string
	CreatedAt       time.Time
	Disabled        bool
}

// IAMUser is a minimal IAM principal record used by the admin endpoints
// (?Action=CreateUser etc.). Stored in the meta backend so identities outlive
// gateway restarts.
type IAMUser struct {
	UserName  string
	UserID    string
	Path      string
	CreatedAt time.Time
}

// ManagedPolicy is an IAM managed-policy document operators can create from
// the embedded console (US-013) and attach to IAM users (US-014). Arn is the
// primary key; Document carries the raw IAM-policy JSON the operator saved.
type ManagedPolicy struct {
	Arn         string
	Name        string
	Path        string
	Description string
	Document    []byte
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// BucketQuota is the per-bucket hard-cap configuration enforced at PUT time
// (US-006). Zero on any field means "unlimited" — matches the AWS / RGW
// shape so an unset quota imposes no limit. A bucket without a configured
// quota row is not subject to any per-bucket cap; user-quota still applies.
type BucketQuota struct {
	MaxBytes          int64
	MaxObjects        int64
	MaxBytesPerObject int64
}

// UserQuota is the per-user hard-cap configuration enforced at CreateBucket
// + PUT time (US-006). Keyed on the IAM user name. Zero on any field means
// "unlimited".
type UserQuota struct {
	MaxBuckets    int32
	TotalMaxBytes int64
}

// BucketStats is the live denormalised per-bucket counter row maintained
// atomically by every PUT / DELETE / multipart Complete / lifecycle expire
// path (US-004). UsedBytes / UsedObjects are signed because the bump path
// accepts negative deltas (DELETE / lifecycle expire). Counter is best-effort
// coherent — drift that a periodic reconcile worker (US-007) corrects.
type BucketStats struct {
	UsedBytes   int64
	UsedObjects int64
	UpdatedAt   time.Time
}

// UsageAggregate is one row in the per-(bucket, storage_class, day) usage
// rollup feed (US-008) consumed by external billing. Day is normalised to
// UTC midnight; ByteSeconds is the integral of UsedBytes over the day
// (v1 approximation: UsedBytes * 86400 from a single sample). ObjectCount*
// summarise the live object count over the day; v1 sets both Avg and Max
// to the same sample value.
type UsageAggregate struct {
	BucketID       uuid.UUID
	Bucket         string
	StorageClass   string
	Day            time.Time
	ByteSeconds    int64
	ObjectCountAvg int64
	ObjectCountMax int64
	ComputedAt     time.Time
}

// Grant is a single ACL grant entry persisted alongside the canned ACL.
// GranteeType is one of: CanonicalUser, Group, AmazonCustomerByEmail.
// Permission is one of: FULL_CONTROL, READ, WRITE, READ_ACP, WRITE_ACP.
type Grant struct {
	GranteeType string
	ID          string
	URI         string
	DisplayName string
	Email       string
	Permission  string
}

const (
	LockModeGovernance = "GOVERNANCE"
	LockModeCompliance = "COMPLIANCE"
)

type Bucket struct {
	Name              string
	ID                uuid.UUID
	Owner             string
	CreatedAt         time.Time
	DefaultClass      string
	Versioning        string
	ACL               string
	ObjectLockEnabled bool
	Region            string
	MfaDelete         string
	ShardCount        int
	TargetShardCount  int
	BackendPresign    bool
	// Placement is the per-bucket cluster routing policy keyed
	// `clusterID → weight`. Nil/empty means "no policy" — chunks route to
	// `$defaultCluster` (zero behavior change for existing buckets). Loaded
	// out-of-band via GetBucketPlacement, not by GetBucket — the GetBucket
	// hot path stays a single buckets-table read. US-001 placement-rebalance.
	Placement map[string]int `json:"placement,omitempty"`
}

const (
	MfaDeleteEnabled  = "Enabled"
	MfaDeleteDisabled = "Disabled"
)

type Object struct {
	BucketID       uuid.UUID
	Key            string
	VersionID      string
	IsLatest       bool
	IsDeleteMarker bool
	// IsNull marks the row as the bucket's "null" version. Set automatically
	// by PutObject when the bucket is in VersioningDisabled mode and by
	// Suspended-mode PUT/DELETE (caller sets IsNull=true on the Object so
	// PutObject takes the Suspended-null path that replaces just the prior
	// null row instead of all rows). Paired with VersionID = NullVersionID.
	// GET ?versionId=null resolves to the row with this flag.
	IsNull         bool
	Size           int64
	ETag           string
	ContentType    string
	StorageClass   string
	Mtime          time.Time
	Manifest       *data.Manifest
	UserMeta       map[string]string
	Tags           map[string]string
	RetainUntil    time.Time
	RetainMode     string
	LegalHold      bool
	Checksums      map[string]string
	SSE            string
	SSECKeyMD5     string
	SSEKey         []byte
	SSEKeyID       string
	RestoreStatus  string
	PartsCount     int
	// PartSizes holds the plaintext byte size of each multipart part in
	// PartNumber order. Empty for single-PUT objects. Populated by
	// CompleteMultipartUpload so GET /<key>?partNumber=N can serve only
	// part N's bytes without revisiting multipart_parts (which is deleted
	// after Complete). Cassandra column: objects.part_sizes list<bigint>.
	PartSizes      []int64
	CacheControl   string
	Expires        string
	ReplicationStatus string
	// ChecksumType is the AWS-defined object-checksum aggregation type:
	// "COMPOSITE" for multipart objects whose composite checksum is
	// HASH(concat(part_digests))-N, "FULL_OBJECT" for multipart objects
	// uploaded with x-amz-checksum-type=FULL_OBJECT on Initiate (the part
	// checksum was computed over the whole object), or empty for single-PUT
	// objects with no aggregation. Surfaced on HEAD/GET via the
	// x-amz-checksum-type response header when ChecksumMode=ENABLED.
	ChecksumType string
}

type ListOptions struct {
	Prefix    string
	Delimiter string
	Marker    string
	Limit     int
}

type ListResult struct {
	Objects        []*Object
	CommonPrefixes []string
	NextMarker     string
	Truncated      bool
}

type ListVersionsResult struct {
	Versions         []*Object
	CommonPrefixes   []string
	NextKeyMarker    string
	NextVersionID    string
	Truncated        bool
}

type MultipartUpload struct {
	BucketID          uuid.UUID
	UploadID          string
	Key               string
	StorageClass      string
	ContentType       string
	InitiatedAt       time.Time
	Status            string
	SSE               string
	SSEKey            []byte
	SSEKeyID          string
	UserMeta          map[string]string
	CacheControl      string
	Expires           string
	ChecksumAlgorithm string
	ChecksumType      string
	// BackendUploadID is set when the gateway maps this Strata multipart
	// session 1:1 onto a backend's own multipart upload (US-010 S3-over-S3
	// pass-through). Empty when running over a chunk-based backend.
	BackendUploadID string
}

type MultipartPart struct {
	PartNumber int
	ETag       string
	Size       int64
	Manifest   *data.Manifest
	Mtime      time.Time
	Checksums  map[string]string
	// BackendETag is the per-part ETag returned by the backend's UploadPart
	// (US-010). Empty when running over a chunk-based backend.
	BackendETag string
}

type CompletePart struct {
	PartNumber int
	ETag       string
}

// preferredChecksumAlgos is the deterministic precedence used by
// BuildPartRange when a multipart part carries more than one stored
// x-amz-checksum-<algo> value. The order mirrors the precedence S3 itself
// surfaces in HEAD responses.
var preferredChecksumAlgos = []string{"CRC64NVME", "SHA256", "SHA1", "CRC32C", "CRC32"}

// BuildPartRange constructs a data.PartRange describing one part of a
// multipart upload at byte-range granularity. offset is the cumulative
// plaintext offset of this part inside the eventual object. The function is
// shared by all meta backends so a future field addition flows through one
// place. p.Checksums may carry multiple algos; the deterministic precedence
// above is consulted before falling back to a stable map iteration.
func BuildPartRange(partNumber int, offset int64, p *MultipartPart) data.PartRange {
	pr := data.PartRange{
		PartNumber: partNumber,
		Offset:     offset,
		Size:       p.Size,
		ETag:       p.ETag,
	}
	for _, algo := range preferredChecksumAlgos {
		if v, ok := p.Checksums[algo]; ok && v != "" {
			pr.ChecksumAlgorithm = algo
			pr.ChecksumValue = v
			return pr
		}
	}
	for algo, v := range p.Checksums {
		if v == "" {
			continue
		}
		pr.ChecksumAlgorithm = algo
		pr.ChecksumValue = v
		return pr
	}
	return pr
}

// GCShardCount is the fixed logical shard fan-out for the GC queue. Writers
// stamp every entry with `shard_id = fnv32a(oid) % GCShardCount`; readers
// pick a runtime shard count via `STRATA_GC_SHARDS` (US-004) and select rows
// where `shard_id % shardCount == myShard`. Decoupling the on-disk shard
// from the runtime shard count means operators can grow / shrink replicas
// without re-keying the queue.
const GCShardCount = 1024

// GCShardID returns the logical (1024-wide) shard for an object id. Identical
// hash is computed at write time on every backend and at read time when a
// backend has to filter post-fetch (e.g. when a query lacks native shard
// locality). Stable: do not change without a queue drain.
func GCShardID(oid string) int {
	h := fnv32aGC(oid)
	return int(h % uint32(GCShardCount))
}

// fnv32aGC is the FNV-1a hash used by GCShardID. Inlined here so the GC
// queue's logical-shard formula stays in this file (callers don't need to
// import "hash/fnv" to derive a shard).
func fnv32aGC(s string) uint32 {
	const (
		offset32 = uint32(2166136261)
		prime32  = uint32(16777619)
	)
	h := offset32
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= prime32
	}
	return h
}

type GCEntry struct {
	Chunk      data.ChunkRef
	EnqueuedAt time.Time
	// ShardID is the logical (0..GCShardCount-1) partition the entry lives
	// on. Computed at write time as `fnv32a(Chunk.OID) % GCShardCount`.
	// Backends that do not persist the column derive it from the oid on
	// read (the value is deterministic).
	ShardID int
}

// NotificationEvent is one buffered S3-event-message payload waiting for a
// notify worker (US-009) to deliver it to its target. One row per matching
// configuration entry — a PUT that satisfies two TopicConfigurations enqueues
// two rows, each carrying the per-target ARN.
type NotificationEvent struct {
	BucketID   uuid.UUID
	Bucket     string
	Key        string
	EventID    string
	EventName  string
	EventTime  time.Time
	ConfigID   string
	TargetType string
	TargetARN  string
	Payload    []byte
}

// NotificationDLQEntry is a row in notify_dlq, written by the notify worker
// after a notification has exhausted its retry budget. The original event is
// embedded; Reason captures the last delivery error and Attempts the number
// of failed sends.
type NotificationDLQEntry struct {
	NotificationEvent
	Attempts   int
	Reason     string
	EnqueuedAt time.Time
}

// AccessLogEntry is one buffered server-access-log row written by the gateway
// HTTP middleware (US-013) when the source bucket has logging configured. The
// strata-access-log worker (US-014) drains this buffer into AWS-format log
// files in the configured target bucket.
type AccessLogEntry struct {
	BucketID     uuid.UUID
	Bucket       string
	EventID      string
	Time         time.Time
	RequestID    string
	Principal    string
	SourceIP     string
	Op           string
	Key          string
	Status       int
	BytesSent    int64
	ObjectSize   int64
	TotalTimeMS  int
	TurnAroundMS int
	Referrer     string
	UserAgent    string
	VersionID    string
}

// AuditEvent is one append-only row in the audit_log table written by the
// gateway HTTP middleware (US-022) for every state-changing request. Read
// operations (GET/HEAD/list) do not emit audit events. BucketID is uuid.Nil
// for global actions (e.g. IAM ?Action=CreateUser) that have no bucket scope;
// in that case Bucket is set to "-" so partition queries still work.
type AuditEvent struct {
	BucketID  uuid.UUID
	Bucket    string
	EventID   string
	Time      time.Time
	Principal string
	Action    string
	Resource  string
	Result    string
	RequestID string
	SourceIP  string
	// UserAgent is the HTTP User-Agent header captured by the audit
	// middleware (US-018). Old rows pre-dating the column read back
	// empty — admin/UI consumers must tolerate "".
	UserAgent string
	// TotalTimeMS is the wall-clock duration (in milliseconds) of the
	// originating HTTP request, captured by the audit middleware (US-003).
	// Powers ListSlowQueries / the Slow Queries debug page. Zero for rows
	// pre-dating the column or for admin-override rows that never hit the
	// HTTP timing path.
	TotalTimeMS int
}

// AuditPartition identifies a single (bucket_id, day) partition of the
// audit_log table. Returned by ListAuditPartitionsBefore for the
// audit-export worker (US-046) so it can read each fully-aged
// partition, write a gzipped JSON-lines export, then delete the partition.
// Day is normalised to UTC midnight; Bucket carries the human-readable
// bucket name when known (or "-" for IAM-scoped rows under uuid.Nil).
type AuditPartition struct {
	BucketID uuid.UUID
	Bucket   string
	Day      time.Time
}

// AuditFilter is the query shape served by the [iam root]-gated /?audit
// endpoint (US-023). Empty Start/End mean "no time filter on that side";
// empty Principal disables principal filtering. BucketScoped=true restricts
// the scan to the partition for BucketID (uuid.Nil is a valid value — IAM
// global rows live there); BucketScoped=false fans out across all buckets.
// Continuation is an opaque token returned by a prior call; both backends
// accept the AuditEvent.EventID of the last record on the previous page.
type AuditFilter struct {
	BucketID     uuid.UUID
	BucketScoped bool
	Principal    string
	Start        time.Time
	End          time.Time
	Limit        int
	Continuation string
}

// ReshardJob is a queued or running per-bucket online reshard pass (US-045).
// The job is created by StartReshard at the same time the bucket's
// TargetShardCount column is set; the worker walks every existing object key
// and re-keys each row under the new partition layout. LastKey is the last
// key the worker successfully copied (resumability) — the job is idempotent
// and resumable from this watermark. Done flips true after CompleteReshard
// has flipped buckets.shard_count and the job is then removed.
type ReshardJob struct {
	BucketID  uuid.UUID
	Bucket    string
	Source    int
	Target    int
	LastKey   string
	Done      bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ShardStat is a per-shard byte+object total returned by
// SampleBucketShardStats. The bucketstats sampler emits these via
// strata_bucket_shard_{bytes,objects} gauges so the Bucket-Shard
// Distribution UI (US-013) can spot key-distribution skew before it bites.
// Only the latest non-delete-marker version of each key contributes.
type ShardStat struct {
	Bytes   int64
	Objects int64
}

// AccessPoint is a named, account-scoped binding to a single bucket carrying
// its own optional bucket policy and PublicAccessBlock configuration. Created
// via the [iam root]-gated ?Action=CreateAccessPoint endpoint (US-040). Name
// is unique per account; Alias is auto-generated as ap-<random12>.
// NetworkOrigin is "Internet" (default) or "VPC". Policy and PAB blobs are
// stored verbatim — interpretation lives in the s3api layer.
type AccessPoint struct {
	Name              string
	BucketID          uuid.UUID
	Bucket            string
	Alias             string
	NetworkOrigin     string
	VPCID             string
	Policy            []byte
	PublicAccessBlock []byte
	CreatedAt         time.Time
}

// ReplicationEvent is one buffered cross-region replication intent waiting
// for the strata-replicator worker (US-012) to copy the source object to the
// destination configured by the matching rule. One row per matching rule —
// a PUT that satisfies two rules enqueues two rows.
type ReplicationEvent struct {
	BucketID            uuid.UUID
	Bucket              string
	Key                 string
	VersionID           string
	EventID             string
	EventName           string
	EventTime           time.Time
	RuleID              string
	DestinationBucket   string
	DestinationEndpoint string
	StorageClass        string
}

type Store interface {
	CreateBucket(ctx context.Context, name, owner, defaultClass string) (*Bucket, error)
	GetBucket(ctx context.Context, name string) (*Bucket, error)
	DeleteBucket(ctx context.Context, name string) error
	ListBuckets(ctx context.Context, owner string) ([]*Bucket, error)
	SetBucketVersioning(ctx context.Context, name, state string) error
	SetBucketACL(ctx context.Context, name, canned string) error
	SetBucketBackendPresign(ctx context.Context, name string, enabled bool) error
	SetBucketGrants(ctx context.Context, bucketID uuid.UUID, grants []Grant) error
	GetBucketGrants(ctx context.Context, bucketID uuid.UUID) ([]Grant, error)
	DeleteBucketGrants(ctx context.Context, bucketID uuid.UUID) error
	SetObjectGrants(ctx context.Context, bucketID uuid.UUID, key, versionID string, grants []Grant) error
	GetObjectGrants(ctx context.Context, bucketID uuid.UUID, key, versionID string) ([]Grant, error)

	PutObject(ctx context.Context, o *Object, versioned bool) error
	GetObject(ctx context.Context, bucketID uuid.UUID, key, versionID string) (*Object, error)
	DeleteObject(ctx context.Context, bucketID uuid.UUID, key, versionID string, versioned bool) (*Object, error)
	// DeleteObjectNullReplacement is the Suspended-mode unversioned DELETE.
	// It atomically removes any prior null-versioned row for the key (LWT
	// IF EXISTS) and writes a fresh null-versioned delete marker. Any
	// TimeUUID-versioned rows for the same key are left untouched.
	DeleteObjectNullReplacement(ctx context.Context, bucketID uuid.UUID, key string) (*Object, error)
	ListObjects(ctx context.Context, bucketID uuid.UUID, opts ListOptions) (*ListResult, error)
	ListObjectVersions(ctx context.Context, bucketID uuid.UUID, opts ListOptions) (*ListVersionsResult, error)
	// SampleBucketShardStats returns per-shard byte/object totals for the
	// bucket. Only the latest non-delete-marker version of each key
	// contributes. shardCount must equal bucket.ShardCount; backends use
	// it to scope per-shard SELECTs (cassandra) or to compute the
	// destination shard from the key (memory, tikv). Used by the
	// bucketstats sampler to publish strata_bucket_shard_bytes /
	// strata_bucket_shard_objects (US-012).
	SampleBucketShardStats(ctx context.Context, bucketID uuid.UUID, shardCount int) (map[int]ShardStat, error)

	SetObjectStorage(ctx context.Context, bucketID uuid.UUID, key, versionID, expectedClass, newClass string, manifest *data.Manifest) (applied bool, err error)

	EnqueueChunkDeletion(ctx context.Context, region string, chunks []data.ChunkRef) error
	ListGCEntries(ctx context.Context, region string, before time.Time, limit int) ([]GCEntry, error)
	// ListGCEntriesShard returns GC entries belonging to the runtime shard
	// `shardID` of `shardCount` total. An entry belongs when its logical
	// (1024-wide) ShardID satisfies `entry.ShardID % shardCount == shardID`.
	// Callers pass shardCount = STRATA_GC_SHARDS (1..1024); shardID is the
	// caller's slot in [0, shardCount). Same time/limit semantics as
	// ListGCEntries. shardCount=1, shardID=0 is functionally identical to
	// ListGCEntries.
	ListGCEntriesShard(ctx context.Context, region string, shardID, shardCount int, before time.Time, limit int) ([]GCEntry, error)
	AckGCEntry(ctx context.Context, region string, entry GCEntry) error

	EnqueueNotification(ctx context.Context, evt *NotificationEvent) error
	ListPendingNotifications(ctx context.Context, bucketID uuid.UUID, limit int) ([]NotificationEvent, error)
	AckNotification(ctx context.Context, evt NotificationEvent) error

	EnqueueNotificationDLQ(ctx context.Context, entry *NotificationDLQEntry) error
	ListNotificationDLQ(ctx context.Context, bucketID uuid.UUID, limit int) ([]NotificationDLQEntry, error)

	EnqueueReplication(ctx context.Context, evt *ReplicationEvent) error
	ListPendingReplications(ctx context.Context, bucketID uuid.UUID, limit int) ([]ReplicationEvent, error)
	AckReplication(ctx context.Context, evt ReplicationEvent) error

	EnqueueAccessLog(ctx context.Context, entry *AccessLogEntry) error
	ListPendingAccessLog(ctx context.Context, bucketID uuid.UUID, limit int) ([]AccessLogEntry, error)
	AckAccessLog(ctx context.Context, entry AccessLogEntry) error

	EnqueueAudit(ctx context.Context, entry *AuditEvent, ttl time.Duration) error
	ListAudit(ctx context.Context, bucketID uuid.UUID, limit int) ([]AuditEvent, error)
	ListAuditFiltered(ctx context.Context, filter AuditFilter) ([]AuditEvent, string, error)
	// ListSlowQueries returns audit rows with TotalTimeMS >= minMs whose
	// Time falls within the trailing `since` window, sorted by TotalTimeMS
	// descending (ties broken by Time desc, then EventID desc). pageToken
	// is the EventID of the last row from the previous page, or "" for
	// the first page. The next-page token is the EventID of the last row
	// returned (or "" when the result set is exhausted).
	ListSlowQueries(ctx context.Context, since time.Duration, minMs int, pageToken string) ([]AuditEvent, string, error)
	// ListAuditPartitionsBefore returns every audit_log (bucket, day)
	// partition whose day is strictly older than the UTC day containing
	// `before`. Used by the audit-export worker to enumerate fully-aged
	// partitions ready for export+delete.
	ListAuditPartitionsBefore(ctx context.Context, before time.Time) ([]AuditPartition, error)
	// ReadAuditPartition returns every row in a single (bucket, day)
	// audit_log partition, sorted ascending by EventID for deterministic
	// export output. The day must already be normalised to UTC midnight
	// (use the value returned from ListAuditPartitionsBefore).
	ReadAuditPartition(ctx context.Context, bucketID uuid.UUID, day time.Time) ([]AuditEvent, error)
	// DeleteAuditPartition drops every row in the given partition. Issued
	// after a successful export upload by the audit-export worker.
	DeleteAuditPartition(ctx context.Context, bucketID uuid.UUID, day time.Time) error

	SetObjectTags(ctx context.Context, bucketID uuid.UUID, key, versionID string, tags map[string]string) error
	GetObjectTags(ctx context.Context, bucketID uuid.UUID, key, versionID string) (map[string]string, error)
	DeleteObjectTags(ctx context.Context, bucketID uuid.UUID, key, versionID string) error

	SetObjectRetention(ctx context.Context, bucketID uuid.UUID, key, versionID, mode string, until time.Time) error
	SetObjectLegalHold(ctx context.Context, bucketID uuid.UUID, key, versionID string, on bool) error
	SetObjectRestoreStatus(ctx context.Context, bucketID uuid.UUID, key, versionID, status string) error

	SetBucketLifecycle(ctx context.Context, bucketID uuid.UUID, xmlBlob []byte) error
	GetBucketLifecycle(ctx context.Context, bucketID uuid.UUID) ([]byte, error)
	DeleteBucketLifecycle(ctx context.Context, bucketID uuid.UUID) error

	SetBucketCORS(ctx context.Context, bucketID uuid.UUID, xmlBlob []byte) error
	GetBucketCORS(ctx context.Context, bucketID uuid.UUID) ([]byte, error)
	DeleteBucketCORS(ctx context.Context, bucketID uuid.UUID) error

	SetBucketPolicy(ctx context.Context, bucketID uuid.UUID, jsonBlob []byte) error
	GetBucketPolicy(ctx context.Context, bucketID uuid.UUID) ([]byte, error)
	DeleteBucketPolicy(ctx context.Context, bucketID uuid.UUID) error

	SetBucketPublicAccessBlock(ctx context.Context, bucketID uuid.UUID, xmlBlob []byte) error
	GetBucketPublicAccessBlock(ctx context.Context, bucketID uuid.UUID) ([]byte, error)
	DeleteBucketPublicAccessBlock(ctx context.Context, bucketID uuid.UUID) error

	SetBucketOwnershipControls(ctx context.Context, bucketID uuid.UUID, xmlBlob []byte) error
	GetBucketOwnershipControls(ctx context.Context, bucketID uuid.UUID) ([]byte, error)
	DeleteBucketOwnershipControls(ctx context.Context, bucketID uuid.UUID) error

	SetBucketEncryption(ctx context.Context, bucketID uuid.UUID, xmlBlob []byte) error
	GetBucketEncryption(ctx context.Context, bucketID uuid.UUID) ([]byte, error)
	DeleteBucketEncryption(ctx context.Context, bucketID uuid.UUID) error

	SetBucketObjectLockEnabled(ctx context.Context, name string, enabled bool) error
	SetBucketRegion(ctx context.Context, name, region string) error
	SetBucketMfaDelete(ctx context.Context, name, state string) error
	SetBucketObjectLockConfig(ctx context.Context, bucketID uuid.UUID, xmlBlob []byte) error
	GetBucketObjectLockConfig(ctx context.Context, bucketID uuid.UUID) ([]byte, error)
	DeleteBucketObjectLockConfig(ctx context.Context, bucketID uuid.UUID) error

	SetBucketNotificationConfig(ctx context.Context, bucketID uuid.UUID, xmlBlob []byte) error
	GetBucketNotificationConfig(ctx context.Context, bucketID uuid.UUID) ([]byte, error)
	DeleteBucketNotificationConfig(ctx context.Context, bucketID uuid.UUID) error

	SetBucketWebsite(ctx context.Context, bucketID uuid.UUID, xmlBlob []byte) error
	GetBucketWebsite(ctx context.Context, bucketID uuid.UUID) ([]byte, error)
	DeleteBucketWebsite(ctx context.Context, bucketID uuid.UUID) error

	SetBucketReplication(ctx context.Context, bucketID uuid.UUID, xmlBlob []byte) error
	GetBucketReplication(ctx context.Context, bucketID uuid.UUID) ([]byte, error)
	DeleteBucketReplication(ctx context.Context, bucketID uuid.UUID) error

	SetBucketLogging(ctx context.Context, bucketID uuid.UUID, xmlBlob []byte) error
	GetBucketLogging(ctx context.Context, bucketID uuid.UUID) ([]byte, error)
	DeleteBucketLogging(ctx context.Context, bucketID uuid.UUID) error

	SetBucketTagging(ctx context.Context, bucketID uuid.UUID, xmlBlob []byte) error
	GetBucketTagging(ctx context.Context, bucketID uuid.UUID) ([]byte, error)
	DeleteBucketTagging(ctx context.Context, bucketID uuid.UUID) error

	// Bucket Placement policy CRUD (US-001 placement-rebalance). Stored
	// per-bucket as a JSON blob. Get returns (nil, nil) when no policy is
	// configured — NOT a sentinel error — so the routing path can fall
	// back to $defaultCluster without branching on errors. SetBucketPlacement
	// applies meta.ValidatePlacement before persisting; cluster-name
	// resolution against the data backend env lives in the admin handler.
	SetBucketPlacement(ctx context.Context, name string, policy map[string]int) error
	GetBucketPlacement(ctx context.Context, name string) (map[string]int, error)
	DeleteBucketPlacement(ctx context.Context, name string) error

	// Quota CRUD (US-001..US-003). Get returns (zero-value, false, nil)
	// when no quota is configured — not a sentinel error — so the gateway's
	// PUT-validate path stays branch-on-bool. Zero on any field of the
	// returned BucketQuota / UserQuota means "unlimited".
	GetBucketQuota(ctx context.Context, bucketID uuid.UUID) (BucketQuota, bool, error)
	SetBucketQuota(ctx context.Context, bucketID uuid.UUID, q BucketQuota) error
	DeleteBucketQuota(ctx context.Context, bucketID uuid.UUID) error
	GetUserQuota(ctx context.Context, userName string) (UserQuota, bool, error)
	SetUserQuota(ctx context.Context, userName string, q UserQuota) error
	DeleteUserQuota(ctx context.Context, userName string) error

	// BucketStats live counter (US-004..US-005). GetBucketStats returns the
	// current row (zero-value when no row exists yet — never an error for the
	// missing case). BumpBucketStats atomically increments the counter and
	// returns the post-update value; negative deltas are allowed (DELETE /
	// lifecycle expire). Cassandra uses LWT and TiKV a pessimistic txn so
	// concurrent bumps serialise without lost updates.
	GetBucketStats(ctx context.Context, bucketID uuid.UUID) (BucketStats, error)
	BumpBucketStats(ctx context.Context, bucketID uuid.UUID, deltaBytes, deltaObjects int64) (BucketStats, error)

	// Usage aggregates (US-008). The leader-elected usage-rollup worker
	// writes one row per (bucketID, storageClass, day) per tick. Day must be
	// normalised to UTC midnight; backends that key on a date column do this
	// for the caller. Reads are inclusive on dayFrom, exclusive on dayTo
	// (half-open) so successive day-aligned queries do not double-count.
	// ListUserUsage walks every bucket owned by userName and returns
	// per-(storageClass, day) sums across them (v1 fan-out — denormalised
	// per-user index is a P3 follow-up).
	WriteUsageAggregate(ctx context.Context, agg UsageAggregate) error
	ListUsageAggregates(ctx context.Context, bucketID uuid.UUID, storageClass string, dayFrom, dayTo time.Time) ([]UsageAggregate, error)
	ListUserUsage(ctx context.Context, userName string, dayFrom, dayTo time.Time) ([]UsageAggregate, error)

	// Inventory configurations are addressed per-bucket by their config id; a
	// bucket may carry multiple at once (AWS allows up to 1,000). The blob is
	// the InventoryConfiguration XML document the client sent.
	SetBucketInventoryConfig(ctx context.Context, bucketID uuid.UUID, configID string, xmlBlob []byte) error
	GetBucketInventoryConfig(ctx context.Context, bucketID uuid.UUID, configID string) ([]byte, error)
	DeleteBucketInventoryConfig(ctx context.Context, bucketID uuid.UUID, configID string) error
	ListBucketInventoryConfigs(ctx context.Context, bucketID uuid.UUID) (map[string][]byte, error)

	// Access points are account-scoped (name unique across the gateway). The
	// passed bucketID filter on ListAccessPoints is uuid.Nil to return all
	// access points; otherwise rows are filtered by binding.
	CreateAccessPoint(ctx context.Context, ap *AccessPoint) error
	GetAccessPoint(ctx context.Context, name string) (*AccessPoint, error)
	GetAccessPointByAlias(ctx context.Context, alias string) (*AccessPoint, error)
	DeleteAccessPoint(ctx context.Context, name string) error
	ListAccessPoints(ctx context.Context, bucketID uuid.UUID) ([]*AccessPoint, error)

	CreateIAMUser(ctx context.Context, u *IAMUser) error
	GetIAMUser(ctx context.Context, userName string) (*IAMUser, error)
	ListIAMUsers(ctx context.Context, pathPrefix string) ([]*IAMUser, error)
	DeleteIAMUser(ctx context.Context, userName string) error

	CreateIAMAccessKey(ctx context.Context, ak *IAMAccessKey) error
	GetIAMAccessKey(ctx context.Context, accessKeyID string) (*IAMAccessKey, error)
	ListIAMAccessKeys(ctx context.Context, userName string) ([]*IAMAccessKey, error)
	DeleteIAMAccessKey(ctx context.Context, accessKeyID string) (*IAMAccessKey, error)
	// UpdateIAMAccessKeyDisabled flips the Disabled bit on the row addressed
	// by accessKeyID. Returns the updated row. Returns ErrIAMAccessKeyNotFound
	// when no row exists. Callers must invalidate any in-memory credential
	// caches that key off accessKeyID after a successful flip — the meta
	// layer carries no cache of its own.
	UpdateIAMAccessKeyDisabled(ctx context.Context, accessKeyID string, disabled bool) (*IAMAccessKey, error)

	// CreateManagedPolicy persists a fresh managed-policy row keyed on
	// policy.Arn. Returns ErrManagedPolicyAlreadyExists if a row with the
	// same arn already exists.
	CreateManagedPolicy(ctx context.Context, policy *ManagedPolicy) error
	// GetManagedPolicy returns the row addressed by arn, or
	// ErrManagedPolicyNotFound.
	GetManagedPolicy(ctx context.Context, arn string) (*ManagedPolicy, error)
	// ListManagedPolicies returns every managed policy whose Path begins
	// with pathPrefix (empty string returns all). Result is sorted by Arn
	// ascending.
	ListManagedPolicies(ctx context.Context, pathPrefix string) ([]*ManagedPolicy, error)
	// UpdateManagedPolicyDocument overwrites the Document blob and bumps
	// UpdatedAt. Returns ErrManagedPolicyNotFound if no row exists.
	UpdateManagedPolicyDocument(ctx context.Context, arn string, document []byte, updatedAt time.Time) error
	// DeleteManagedPolicy deletes the row addressed by arn. Returns
	// ErrPolicyAttached when at least one row in iam_user_policies (or
	// equivalent backend index) references arn — callers must detach
	// first.
	DeleteManagedPolicy(ctx context.Context, arn string) error

	// AttachUserPolicy records that userName has policyArn attached.
	// Returns ErrIAMUserNotFound if the user does not exist,
	// ErrManagedPolicyNotFound if the policy does not exist,
	// ErrUserPolicyAlreadyAttached if the attachment row already exists.
	AttachUserPolicy(ctx context.Context, userName, policyArn string) error
	// DetachUserPolicy removes the attachment between userName and
	// policyArn. Returns ErrUserPolicyNotAttached if the row does not
	// exist.
	DetachUserPolicy(ctx context.Context, userName, policyArn string) error
	// ListUserPolicies returns every policy ARN attached to userName,
	// sorted ascending. Returns ErrIAMUserNotFound if the user does not
	// exist.
	ListUserPolicies(ctx context.Context, userName string) ([]string, error)
	// ListPolicyUsers returns every user name attached to policyArn, sorted
	// ascending. Returns an empty slice (no error) when the policy has no
	// attachments. Returns ErrManagedPolicyNotFound if the policy does not
	// exist. The inverse of ListUserPolicies — backed by the same per-policy
	// inverse-index used by DeleteManagedPolicy's attachment check (US-013).
	ListPolicyUsers(ctx context.Context, policyArn string) ([]string, error)

	CreateMultipartUpload(ctx context.Context, mu *MultipartUpload) error
	GetMultipartUpload(ctx context.Context, bucketID uuid.UUID, uploadID string) (*MultipartUpload, error)
	ListMultipartUploads(ctx context.Context, bucketID uuid.UUID, prefix string, limit int) ([]*MultipartUpload, error)
	SavePart(ctx context.Context, bucketID uuid.UUID, uploadID string, part *MultipartPart) error
	ListParts(ctx context.Context, bucketID uuid.UUID, uploadID string) ([]*MultipartPart, error)
	CompleteMultipartUpload(ctx context.Context, obj *Object, uploadID string, parts []CompletePart, versioned bool) (orphans []*data.Manifest, err error)
	AbortMultipartUpload(ctx context.Context, bucketID uuid.UUID, uploadID string) ([]*data.Manifest, error)

	RecordMultipartCompletion(ctx context.Context, rec *MultipartCompletion, ttl time.Duration) error
	GetMultipartCompletion(ctx context.Context, bucketID uuid.UUID, uploadID string) (*MultipartCompletion, error)

	UpdateObjectSSEWrap(ctx context.Context, bucketID uuid.UUID, key, versionID string, wrapped []byte, keyID string) error
	UpdateMultipartUploadSSEWrap(ctx context.Context, bucketID uuid.UUID, uploadID string, wrapped []byte, keyID string) error
	SetRewrapProgress(ctx context.Context, p *RewrapProgress) error
	GetRewrapProgress(ctx context.Context, bucketID uuid.UUID) (*RewrapProgress, error)

	// GetObjectManifestRaw returns the raw, persisted manifest blob for the
	// given object version. Used by the manifest rewriter (US-049) to detect
	// JSON-encoded rows and convert them to protobuf in place.
	GetObjectManifestRaw(ctx context.Context, bucketID uuid.UUID, key, versionID string) ([]byte, error)
	// UpdateObjectManifestRaw overwrites the raw manifest blob for the given
	// object version. Callers are responsible for re-encoding correctly; the
	// store does not validate the bytes. Used by the rewriter to flip a
	// JSON-encoded manifest to protobuf without disturbing other columns.
	UpdateObjectManifestRaw(ctx context.Context, bucketID uuid.UUID, key, versionID string, raw []byte) error

	SetObjectReplicationStatus(ctx context.Context, bucketID uuid.UUID, key, versionID, status string) error

	// StartReshard queues an online shard-resize for bucketID with the given
	// target partition count and writes the bucket's TargetShardCount column.
	// Returns ErrReshardInProgress when a reshard is already queued or
	// running, ErrReshardInvalidTarget when target is not a positive integer
	// strictly greater than the current shard count.
	StartReshard(ctx context.Context, bucketID uuid.UUID, target int) (*ReshardJob, error)
	// GetReshardJob returns the active or queued job, or ErrReshardNotFound.
	GetReshardJob(ctx context.Context, bucketID uuid.UUID) (*ReshardJob, error)
	// UpdateReshardJob persists a watermark/state update. The worker calls
	// this after each batch so a crash resumes from LastKey.
	UpdateReshardJob(ctx context.Context, job *ReshardJob) error
	// CompleteReshard atomically flips buckets.shard_count to the job's
	// target, clears TargetShardCount, marks the job Done and deletes it.
	CompleteReshard(ctx context.Context, bucketID uuid.UUID) error
	// ListReshardJobs returns every queued or running reshard job for the
	// gateway. The reshard worker calls this on each tick.
	ListReshardJobs(ctx context.Context) ([]*ReshardJob, error)

	// CreateAdminJob persists a fresh AdminJob row keyed on job.ID. Returns
	// ErrAdminJobAlreadyExists if a row with the same ID already exists.
	CreateAdminJob(ctx context.Context, job *AdminJob) error
	// GetAdminJob returns the row addressed by id, or ErrAdminJobNotFound.
	GetAdminJob(ctx context.Context, id string) (*AdminJob, error)
	// UpdateAdminJob overwrites the State/Message/Deleted/UpdatedAt/
	// FinishedAt columns. Returns ErrAdminJobNotFound when no row exists.
	UpdateAdminJob(ctx context.Context, job *AdminJob) error

	Close() error
}

// MetaHealthReport is the operator-facing snapshot returned by HealthProbe.
// One row per peer (Cassandra: system.peers + system.local merge; TiKV: PD
// /pd/api/v1/stores; memory: a single self-row). Warnings carry cluster-wide
// anomalies that don't fit on a per-node row (schema drift, raft-leader
// imbalance, etc.).
type MetaHealthReport struct {
	Backend           string       `json:"backend"`
	Nodes             []NodeStatus `json:"nodes"`
	ReplicationFactor int          `json:"replication_factor"`
	Warnings          []string     `json:"warnings,omitempty"`
}

// NodeStatus is a single peer/store row in MetaHealthReport.
type NodeStatus struct {
	Address       string `json:"address"`
	State         string `json:"state"`
	SchemaVersion string `json:"schema_version,omitempty"`
	DataCenter    string `json:"data_center,omitempty"`
	Rack          string `json:"rack,omitempty"`
}

// HealthProbe is the optional capability surface that lets the storage page
// (US-001 of the web-ui-storage-status cycle) render meta backend topology.
// Cassandra/Scylla, TiKV, and the in-memory store all implement it; the
// gateway adminapi type-asserts the live Store and surfaces the report at
// GET /admin/v1/storage/meta.
type HealthProbe interface {
	MetaHealth(ctx context.Context) (*MetaHealthReport, error)
}

// RangeScanStore is the optional capability surface for backends whose
// physical layout supports a single ordered range scan over (bucket, prefix).
// Backends that implement it advertise to the gateway "ListObjects can be
// served by one continuous scan instead of N-way fan-out + heap-merge". The
// gateway type-asserts the live Store at the dispatch site (see
// internal/s3api/server.go::listObjects) and routes through ScanObjects when
// available, falling back to Store.ListObjects otherwise.
//
// Memory and TiKV both ship a true single-shot range-scan implementation —
// memory because its in-process tree-map is naturally ordered, TiKV because
// its KV layout (US-002 key encoding) gives a globally sorted byte-string
// keyspace. Cassandra deliberately does NOT implement this interface: the
// objects table is partitioned by (bucket_id, shard) so any prefix scan must
// fan out across N partitions and heap-merge by clustering order — that
// fan-out IS the implementation in cassandra.Store.ListObjects, and hoisting
// it to a "single range scan" name would just hide the same code.
//
// ScanObjects accepts the same meta.ListOptions shape Store.ListObjects
// takes; the result shape is identical so the dispatch site is a one-line
// type-assertion fork.
type RangeScanStore interface {
	Store
	ScanObjects(ctx context.Context, bucketID uuid.UUID, opts ListOptions) (*ListResult, error)
}

func IsVersioningActive(state string) bool {
	return state == VersioningEnabled || state == VersioningSuspended
}

// IsValidShardCount reports whether n is acceptable as a bucket shard count.
// Constraints: positive and a power of two so cassandra fnv-modulo splits
// remain stable when the count grows by a factor of 2 (every old shard
// either stays or splits cleanly into two new ones).
func IsValidShardCount(n int) bool {
	return n > 0 && (n&(n-1)) == 0
}
