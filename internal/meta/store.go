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
	CacheControl   string
	Expires        string
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
	ListObjects(ctx context.Context, bucketID uuid.UUID, opts ListOptions) (*ListResult, error)
	ListObjectVersions(ctx context.Context, bucketID uuid.UUID, opts ListOptions) (*ListVersionsResult, error)

	SetObjectStorage(ctx context.Context, bucketID uuid.UUID, key, versionID, expectedClass, newClass string, manifest *data.Manifest) (applied bool, err error)

	EnqueueChunkDeletion(ctx context.Context, region string, chunks []data.ChunkRef) error
	ListGCEntries(ctx context.Context, region string, before time.Time, limit int) ([]GCEntry, error)
	AckGCEntry(ctx context.Context, region string, entry GCEntry) error

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

	Close() error
}

func IsVersioningActive(state string) bool {
	return state == VersioningEnabled || state == VersioningSuspended
}
