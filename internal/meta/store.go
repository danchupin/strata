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
)

const (
	LockModeGovernance = "GOVERNANCE"
	LockModeCompliance = "COMPLIANCE"
)

type Bucket struct {
	Name         string
	ID           uuid.UUID
	Owner        string
	CreatedAt    time.Time
	DefaultClass string
	Versioning   string
}

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
	BucketID     uuid.UUID
	UploadID     string
	Key          string
	StorageClass string
	ContentType  string
	InitiatedAt  time.Time
	Status       string
}

type MultipartPart struct {
	PartNumber int
	ETag       string
	Size       int64
	Manifest   *data.Manifest
	Mtime      time.Time
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

	SetBucketLifecycle(ctx context.Context, bucketID uuid.UUID, xmlBlob []byte) error
	GetBucketLifecycle(ctx context.Context, bucketID uuid.UUID) ([]byte, error)
	DeleteBucketLifecycle(ctx context.Context, bucketID uuid.UUID) error

	CreateMultipartUpload(ctx context.Context, mu *MultipartUpload) error
	GetMultipartUpload(ctx context.Context, bucketID uuid.UUID, uploadID string) (*MultipartUpload, error)
	ListMultipartUploads(ctx context.Context, bucketID uuid.UUID, prefix string, limit int) ([]*MultipartUpload, error)
	SavePart(ctx context.Context, bucketID uuid.UUID, uploadID string, part *MultipartPart) error
	ListParts(ctx context.Context, bucketID uuid.UUID, uploadID string) ([]*MultipartPart, error)
	CompleteMultipartUpload(ctx context.Context, obj *Object, uploadID string, parts []CompletePart, versioned bool) (orphans []*data.Manifest, err error)
	AbortMultipartUpload(ctx context.Context, bucketID uuid.UUID, uploadID string) ([]*data.Manifest, error)

	Close() error
}

func IsVersioningActive(state string) bool {
	return state == VersioningEnabled || state == VersioningSuspended
}
