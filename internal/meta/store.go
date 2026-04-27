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
	ErrMultipartCompletionNotFound = errors.New("multipart completion record not found or expired")
	ErrNoRewrapProgress        = errors.New("no rewrap progress recorded for bucket")
)

// RewrapProgress tracks a master-key rewrap pass for a single bucket. Used by
// cmd/strata-rewrap for resumability across runs.
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
	// ShardCount is the active partition count for this bucket's objects
	// table. Defaults to the gateway's STRATA_BUCKET_SHARDS at CreateBucket
	// time. Once persisted, only CompleteReshard rotates it.
	ShardCount int
	// TargetShardCount is the target partition count of an in-progress
	// reshard (US-045). Zero when no reshard is queued or running. While
	// non-zero, ListObjects unions the active+target layouts so clients
	// see no gap during the rewrite.
	TargetShardCount int
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
	// ChecksumType is the client-requested aggregation mode from the Initiate
	// x-amz-checksum-type header ("FULL_OBJECT" or "COMPOSITE"). Empty when
	// the client did not specify it; CompleteMultipartUpload then defaults to
	// COMPOSITE when ChecksumAlgorithm is non-empty. Persisted so Complete on
	// a different gateway instance can preserve the client's choice.
	ChecksumType string
}

type MultipartPart struct {
	PartNumber int
	ETag       string
	Size       int64
	Manifest   *data.Manifest
	Mtime      time.Time
	Checksums  map[string]string
}

type CompletePart struct {
	PartNumber int
	ETag       string
}

type GCEntry struct {
	Chunk      data.ChunkRef
	EnqueuedAt time.Time
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
}

// AuditPartition identifies a single (bucket_id, day) partition of the
// audit_log table. Returned by ListAuditPartitionsBefore for the
// strata-audit-export worker (US-046) so it can read each fully-aged
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

	SetObjectStorage(ctx context.Context, bucketID uuid.UUID, key, versionID, expectedClass, newClass string, manifest *data.Manifest) (applied bool, err error)

	EnqueueChunkDeletion(ctx context.Context, region string, chunks []data.ChunkRef) error
	ListGCEntries(ctx context.Context, region string, before time.Time, limit int) ([]GCEntry, error)
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
	// ListAuditPartitionsBefore returns every audit_log (bucket, day)
	// partition whose day is strictly older than the UTC day containing
	// `before`. Used by strata-audit-export to enumerate fully-aged
	// partitions ready for export+delete.
	ListAuditPartitionsBefore(ctx context.Context, before time.Time) ([]AuditPartition, error)
	// ReadAuditPartition returns every row in a single (bucket, day)
	// audit_log partition, sorted ascending by EventID for deterministic
	// export output. The day must already be normalised to UTC midnight
	// (use the value returned from ListAuditPartitionsBefore).
	ReadAuditPartition(ctx context.Context, bucketID uuid.UUID, day time.Time) ([]AuditEvent, error)
	// DeleteAuditPartition drops every row in the given partition. Issued
	// after a successful export upload by strata-audit-export.
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

	Close() error
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
