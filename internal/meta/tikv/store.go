// Package tikv is the TiKV-backed implementation of meta.Store.
//
// US-001 lands the skeleton: every method is a stub returning
// errors.ErrUnsupported so the package compiles and satisfies the
// meta.Store interface contract while subsequent stories fill in the
// real implementations (key encoding US-002, bucket CRUD US-003, ...).
//
// STRATA_META_BACKEND=tikv is reserved but NOT yet wired into
// internal/serverapp's dispatch — production routing lands in US-015.
package tikv

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	// Blank import keeps github.com/tikv/client-go/v2 in go.sum / go.mod
	// across go-mod-tidy passes until US-003 imports its txn API for
	// real. Picked the smallest leaf package so we do not pull the full
	// txnkv tree until needed.
	_ "github.com/tikv/client-go/v2/config"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/meta"
)

// Config holds connection parameters for a TiKV cluster. Only the PD
// (Placement Driver) endpoint list is required; later stories may add
// TLS, timeouts, retry knobs.
type Config struct {
	PDEndpoints []string
}

// Store is the TiKV-backed meta.Store. The struct is intentionally bare
// at this stage — US-003 onwards adds the txnkv client + sweeper + etc.
type Store struct {
	cfg Config
}

// Open returns a Store stub. Real PD/TxnKV wiring lands with the first
// story that needs round-trip CRUD (US-003).
func Open(cfg Config) (*Store, error) {
	return &Store{cfg: cfg}, nil
}

// Close releases any resources held by the Store. The stub holds none.
func (s *Store) Close() error { return nil }

// Probe is the readiness probe consumed by the gateway /readyz endpoint
// (see internal/health.Handler wiring in serverapp). Returns
// ErrUnsupported until US-003 connects to PD.
func (s *Store) Probe(ctx context.Context) error { return errors.ErrUnsupported }

func (s *Store) CreateBucket(ctx context.Context, name, owner, defaultClass string) (*meta.Bucket, error) {
	return nil, errors.ErrUnsupported
}

func (s *Store) GetBucket(ctx context.Context, name string) (*meta.Bucket, error) {
	return nil, errors.ErrUnsupported
}

func (s *Store) DeleteBucket(ctx context.Context, name string) error {
	return errors.ErrUnsupported
}

func (s *Store) ListBuckets(ctx context.Context, owner string) ([]*meta.Bucket, error) {
	return nil, errors.ErrUnsupported
}

func (s *Store) SetBucketVersioning(ctx context.Context, name, state string) error {
	return errors.ErrUnsupported
}

func (s *Store) SetBucketACL(ctx context.Context, name, canned string) error {
	return errors.ErrUnsupported
}

func (s *Store) SetBucketGrants(ctx context.Context, bucketID uuid.UUID, grants []meta.Grant) error {
	return errors.ErrUnsupported
}

func (s *Store) GetBucketGrants(ctx context.Context, bucketID uuid.UUID) ([]meta.Grant, error) {
	return nil, errors.ErrUnsupported
}

func (s *Store) DeleteBucketGrants(ctx context.Context, bucketID uuid.UUID) error {
	return errors.ErrUnsupported
}

func (s *Store) SetObjectGrants(ctx context.Context, bucketID uuid.UUID, key, versionID string, grants []meta.Grant) error {
	return errors.ErrUnsupported
}

func (s *Store) GetObjectGrants(ctx context.Context, bucketID uuid.UUID, key, versionID string) ([]meta.Grant, error) {
	return nil, errors.ErrUnsupported
}

func (s *Store) PutObject(ctx context.Context, o *meta.Object, versioned bool) error {
	return errors.ErrUnsupported
}

func (s *Store) GetObject(ctx context.Context, bucketID uuid.UUID, key, versionID string) (*meta.Object, error) {
	return nil, errors.ErrUnsupported
}

func (s *Store) DeleteObject(ctx context.Context, bucketID uuid.UUID, key, versionID string, versioned bool) (*meta.Object, error) {
	return nil, errors.ErrUnsupported
}

func (s *Store) DeleteObjectNullReplacement(ctx context.Context, bucketID uuid.UUID, key string) (*meta.Object, error) {
	return nil, errors.ErrUnsupported
}

func (s *Store) ListObjects(ctx context.Context, bucketID uuid.UUID, opts meta.ListOptions) (*meta.ListResult, error) {
	return nil, errors.ErrUnsupported
}

func (s *Store) ListObjectVersions(ctx context.Context, bucketID uuid.UUID, opts meta.ListOptions) (*meta.ListVersionsResult, error) {
	return nil, errors.ErrUnsupported
}

func (s *Store) SetObjectStorage(ctx context.Context, bucketID uuid.UUID, key, versionID, expectedClass, newClass string, manifest *data.Manifest) (bool, error) {
	return false, errors.ErrUnsupported
}

func (s *Store) EnqueueChunkDeletion(ctx context.Context, region string, chunks []data.ChunkRef) error {
	return errors.ErrUnsupported
}

func (s *Store) ListGCEntries(ctx context.Context, region string, before time.Time, limit int) ([]meta.GCEntry, error) {
	return nil, errors.ErrUnsupported
}

func (s *Store) AckGCEntry(ctx context.Context, region string, entry meta.GCEntry) error {
	return errors.ErrUnsupported
}

func (s *Store) EnqueueNotification(ctx context.Context, evt *meta.NotificationEvent) error {
	return errors.ErrUnsupported
}

func (s *Store) ListPendingNotifications(ctx context.Context, bucketID uuid.UUID, limit int) ([]meta.NotificationEvent, error) {
	return nil, errors.ErrUnsupported
}

func (s *Store) AckNotification(ctx context.Context, evt meta.NotificationEvent) error {
	return errors.ErrUnsupported
}

func (s *Store) EnqueueNotificationDLQ(ctx context.Context, entry *meta.NotificationDLQEntry) error {
	return errors.ErrUnsupported
}

func (s *Store) ListNotificationDLQ(ctx context.Context, bucketID uuid.UUID, limit int) ([]meta.NotificationDLQEntry, error) {
	return nil, errors.ErrUnsupported
}

func (s *Store) EnqueueReplication(ctx context.Context, evt *meta.ReplicationEvent) error {
	return errors.ErrUnsupported
}

func (s *Store) ListPendingReplications(ctx context.Context, bucketID uuid.UUID, limit int) ([]meta.ReplicationEvent, error) {
	return nil, errors.ErrUnsupported
}

func (s *Store) AckReplication(ctx context.Context, evt meta.ReplicationEvent) error {
	return errors.ErrUnsupported
}

func (s *Store) EnqueueAccessLog(ctx context.Context, entry *meta.AccessLogEntry) error {
	return errors.ErrUnsupported
}

func (s *Store) ListPendingAccessLog(ctx context.Context, bucketID uuid.UUID, limit int) ([]meta.AccessLogEntry, error) {
	return nil, errors.ErrUnsupported
}

func (s *Store) AckAccessLog(ctx context.Context, entry meta.AccessLogEntry) error {
	return errors.ErrUnsupported
}

func (s *Store) EnqueueAudit(ctx context.Context, entry *meta.AuditEvent, ttl time.Duration) error {
	return errors.ErrUnsupported
}

func (s *Store) ListAudit(ctx context.Context, bucketID uuid.UUID, limit int) ([]meta.AuditEvent, error) {
	return nil, errors.ErrUnsupported
}

func (s *Store) ListAuditFiltered(ctx context.Context, filter meta.AuditFilter) ([]meta.AuditEvent, string, error) {
	return nil, "", errors.ErrUnsupported
}

func (s *Store) ListAuditPartitionsBefore(ctx context.Context, before time.Time) ([]meta.AuditPartition, error) {
	return nil, errors.ErrUnsupported
}

func (s *Store) ReadAuditPartition(ctx context.Context, bucketID uuid.UUID, day time.Time) ([]meta.AuditEvent, error) {
	return nil, errors.ErrUnsupported
}

func (s *Store) DeleteAuditPartition(ctx context.Context, bucketID uuid.UUID, day time.Time) error {
	return errors.ErrUnsupported
}

func (s *Store) SetObjectTags(ctx context.Context, bucketID uuid.UUID, key, versionID string, tags map[string]string) error {
	return errors.ErrUnsupported
}

func (s *Store) GetObjectTags(ctx context.Context, bucketID uuid.UUID, key, versionID string) (map[string]string, error) {
	return nil, errors.ErrUnsupported
}

func (s *Store) DeleteObjectTags(ctx context.Context, bucketID uuid.UUID, key, versionID string) error {
	return errors.ErrUnsupported
}

func (s *Store) SetObjectRetention(ctx context.Context, bucketID uuid.UUID, key, versionID, mode string, until time.Time) error {
	return errors.ErrUnsupported
}

func (s *Store) SetObjectLegalHold(ctx context.Context, bucketID uuid.UUID, key, versionID string, on bool) error {
	return errors.ErrUnsupported
}

func (s *Store) SetObjectRestoreStatus(ctx context.Context, bucketID uuid.UUID, key, versionID, status string) error {
	return errors.ErrUnsupported
}

func (s *Store) SetBucketLifecycle(ctx context.Context, bucketID uuid.UUID, xmlBlob []byte) error {
	return errors.ErrUnsupported
}

func (s *Store) GetBucketLifecycle(ctx context.Context, bucketID uuid.UUID) ([]byte, error) {
	return nil, errors.ErrUnsupported
}

func (s *Store) DeleteBucketLifecycle(ctx context.Context, bucketID uuid.UUID) error {
	return errors.ErrUnsupported
}

func (s *Store) SetBucketCORS(ctx context.Context, bucketID uuid.UUID, xmlBlob []byte) error {
	return errors.ErrUnsupported
}

func (s *Store) GetBucketCORS(ctx context.Context, bucketID uuid.UUID) ([]byte, error) {
	return nil, errors.ErrUnsupported
}

func (s *Store) DeleteBucketCORS(ctx context.Context, bucketID uuid.UUID) error {
	return errors.ErrUnsupported
}

func (s *Store) SetBucketPolicy(ctx context.Context, bucketID uuid.UUID, jsonBlob []byte) error {
	return errors.ErrUnsupported
}

func (s *Store) GetBucketPolicy(ctx context.Context, bucketID uuid.UUID) ([]byte, error) {
	return nil, errors.ErrUnsupported
}

func (s *Store) DeleteBucketPolicy(ctx context.Context, bucketID uuid.UUID) error {
	return errors.ErrUnsupported
}

func (s *Store) SetBucketPublicAccessBlock(ctx context.Context, bucketID uuid.UUID, xmlBlob []byte) error {
	return errors.ErrUnsupported
}

func (s *Store) GetBucketPublicAccessBlock(ctx context.Context, bucketID uuid.UUID) ([]byte, error) {
	return nil, errors.ErrUnsupported
}

func (s *Store) DeleteBucketPublicAccessBlock(ctx context.Context, bucketID uuid.UUID) error {
	return errors.ErrUnsupported
}

func (s *Store) SetBucketOwnershipControls(ctx context.Context, bucketID uuid.UUID, xmlBlob []byte) error {
	return errors.ErrUnsupported
}

func (s *Store) GetBucketOwnershipControls(ctx context.Context, bucketID uuid.UUID) ([]byte, error) {
	return nil, errors.ErrUnsupported
}

func (s *Store) DeleteBucketOwnershipControls(ctx context.Context, bucketID uuid.UUID) error {
	return errors.ErrUnsupported
}

func (s *Store) SetBucketEncryption(ctx context.Context, bucketID uuid.UUID, xmlBlob []byte) error {
	return errors.ErrUnsupported
}

func (s *Store) GetBucketEncryption(ctx context.Context, bucketID uuid.UUID) ([]byte, error) {
	return nil, errors.ErrUnsupported
}

func (s *Store) DeleteBucketEncryption(ctx context.Context, bucketID uuid.UUID) error {
	return errors.ErrUnsupported
}

func (s *Store) SetBucketObjectLockEnabled(ctx context.Context, name string, enabled bool) error {
	return errors.ErrUnsupported
}

func (s *Store) SetBucketRegion(ctx context.Context, name, region string) error {
	return errors.ErrUnsupported
}

func (s *Store) SetBucketMfaDelete(ctx context.Context, name, state string) error {
	return errors.ErrUnsupported
}

func (s *Store) SetBucketObjectLockConfig(ctx context.Context, bucketID uuid.UUID, xmlBlob []byte) error {
	return errors.ErrUnsupported
}

func (s *Store) GetBucketObjectLockConfig(ctx context.Context, bucketID uuid.UUID) ([]byte, error) {
	return nil, errors.ErrUnsupported
}

func (s *Store) DeleteBucketObjectLockConfig(ctx context.Context, bucketID uuid.UUID) error {
	return errors.ErrUnsupported
}

func (s *Store) SetBucketNotificationConfig(ctx context.Context, bucketID uuid.UUID, xmlBlob []byte) error {
	return errors.ErrUnsupported
}

func (s *Store) GetBucketNotificationConfig(ctx context.Context, bucketID uuid.UUID) ([]byte, error) {
	return nil, errors.ErrUnsupported
}

func (s *Store) DeleteBucketNotificationConfig(ctx context.Context, bucketID uuid.UUID) error {
	return errors.ErrUnsupported
}

func (s *Store) SetBucketWebsite(ctx context.Context, bucketID uuid.UUID, xmlBlob []byte) error {
	return errors.ErrUnsupported
}

func (s *Store) GetBucketWebsite(ctx context.Context, bucketID uuid.UUID) ([]byte, error) {
	return nil, errors.ErrUnsupported
}

func (s *Store) DeleteBucketWebsite(ctx context.Context, bucketID uuid.UUID) error {
	return errors.ErrUnsupported
}

func (s *Store) SetBucketReplication(ctx context.Context, bucketID uuid.UUID, xmlBlob []byte) error {
	return errors.ErrUnsupported
}

func (s *Store) GetBucketReplication(ctx context.Context, bucketID uuid.UUID) ([]byte, error) {
	return nil, errors.ErrUnsupported
}

func (s *Store) DeleteBucketReplication(ctx context.Context, bucketID uuid.UUID) error {
	return errors.ErrUnsupported
}

func (s *Store) SetBucketLogging(ctx context.Context, bucketID uuid.UUID, xmlBlob []byte) error {
	return errors.ErrUnsupported
}

func (s *Store) GetBucketLogging(ctx context.Context, bucketID uuid.UUID) ([]byte, error) {
	return nil, errors.ErrUnsupported
}

func (s *Store) DeleteBucketLogging(ctx context.Context, bucketID uuid.UUID) error {
	return errors.ErrUnsupported
}

func (s *Store) SetBucketTagging(ctx context.Context, bucketID uuid.UUID, xmlBlob []byte) error {
	return errors.ErrUnsupported
}

func (s *Store) GetBucketTagging(ctx context.Context, bucketID uuid.UUID) ([]byte, error) {
	return nil, errors.ErrUnsupported
}

func (s *Store) DeleteBucketTagging(ctx context.Context, bucketID uuid.UUID) error {
	return errors.ErrUnsupported
}

func (s *Store) SetBucketInventoryConfig(ctx context.Context, bucketID uuid.UUID, configID string, xmlBlob []byte) error {
	return errors.ErrUnsupported
}

func (s *Store) GetBucketInventoryConfig(ctx context.Context, bucketID uuid.UUID, configID string) ([]byte, error) {
	return nil, errors.ErrUnsupported
}

func (s *Store) DeleteBucketInventoryConfig(ctx context.Context, bucketID uuid.UUID, configID string) error {
	return errors.ErrUnsupported
}

func (s *Store) ListBucketInventoryConfigs(ctx context.Context, bucketID uuid.UUID) (map[string][]byte, error) {
	return nil, errors.ErrUnsupported
}

func (s *Store) CreateAccessPoint(ctx context.Context, ap *meta.AccessPoint) error {
	return errors.ErrUnsupported
}

func (s *Store) GetAccessPoint(ctx context.Context, name string) (*meta.AccessPoint, error) {
	return nil, errors.ErrUnsupported
}

func (s *Store) GetAccessPointByAlias(ctx context.Context, alias string) (*meta.AccessPoint, error) {
	return nil, errors.ErrUnsupported
}

func (s *Store) DeleteAccessPoint(ctx context.Context, name string) error {
	return errors.ErrUnsupported
}

func (s *Store) ListAccessPoints(ctx context.Context, bucketID uuid.UUID) ([]*meta.AccessPoint, error) {
	return nil, errors.ErrUnsupported
}

func (s *Store) CreateIAMUser(ctx context.Context, u *meta.IAMUser) error {
	return errors.ErrUnsupported
}

func (s *Store) GetIAMUser(ctx context.Context, userName string) (*meta.IAMUser, error) {
	return nil, errors.ErrUnsupported
}

func (s *Store) ListIAMUsers(ctx context.Context, pathPrefix string) ([]*meta.IAMUser, error) {
	return nil, errors.ErrUnsupported
}

func (s *Store) DeleteIAMUser(ctx context.Context, userName string) error {
	return errors.ErrUnsupported
}

func (s *Store) CreateIAMAccessKey(ctx context.Context, ak *meta.IAMAccessKey) error {
	return errors.ErrUnsupported
}

func (s *Store) GetIAMAccessKey(ctx context.Context, accessKeyID string) (*meta.IAMAccessKey, error) {
	return nil, errors.ErrUnsupported
}

func (s *Store) ListIAMAccessKeys(ctx context.Context, userName string) ([]*meta.IAMAccessKey, error) {
	return nil, errors.ErrUnsupported
}

func (s *Store) DeleteIAMAccessKey(ctx context.Context, accessKeyID string) (*meta.IAMAccessKey, error) {
	return nil, errors.ErrUnsupported
}

func (s *Store) CreateMultipartUpload(ctx context.Context, mu *meta.MultipartUpload) error {
	return errors.ErrUnsupported
}

func (s *Store) GetMultipartUpload(ctx context.Context, bucketID uuid.UUID, uploadID string) (*meta.MultipartUpload, error) {
	return nil, errors.ErrUnsupported
}

func (s *Store) ListMultipartUploads(ctx context.Context, bucketID uuid.UUID, prefix string, limit int) ([]*meta.MultipartUpload, error) {
	return nil, errors.ErrUnsupported
}

func (s *Store) SavePart(ctx context.Context, bucketID uuid.UUID, uploadID string, part *meta.MultipartPart) error {
	return errors.ErrUnsupported
}

func (s *Store) ListParts(ctx context.Context, bucketID uuid.UUID, uploadID string) ([]*meta.MultipartPart, error) {
	return nil, errors.ErrUnsupported
}

func (s *Store) CompleteMultipartUpload(ctx context.Context, obj *meta.Object, uploadID string, parts []meta.CompletePart, versioned bool) ([]*data.Manifest, error) {
	return nil, errors.ErrUnsupported
}

func (s *Store) AbortMultipartUpload(ctx context.Context, bucketID uuid.UUID, uploadID string) ([]*data.Manifest, error) {
	return nil, errors.ErrUnsupported
}

func (s *Store) RecordMultipartCompletion(ctx context.Context, rec *meta.MultipartCompletion, ttl time.Duration) error {
	return errors.ErrUnsupported
}

func (s *Store) GetMultipartCompletion(ctx context.Context, bucketID uuid.UUID, uploadID string) (*meta.MultipartCompletion, error) {
	return nil, errors.ErrUnsupported
}

func (s *Store) UpdateObjectSSEWrap(ctx context.Context, bucketID uuid.UUID, key, versionID string, wrapped []byte, keyID string) error {
	return errors.ErrUnsupported
}

func (s *Store) UpdateMultipartUploadSSEWrap(ctx context.Context, bucketID uuid.UUID, uploadID string, wrapped []byte, keyID string) error {
	return errors.ErrUnsupported
}

func (s *Store) SetRewrapProgress(ctx context.Context, p *meta.RewrapProgress) error {
	return errors.ErrUnsupported
}

func (s *Store) GetRewrapProgress(ctx context.Context, bucketID uuid.UUID) (*meta.RewrapProgress, error) {
	return nil, errors.ErrUnsupported
}

func (s *Store) GetObjectManifestRaw(ctx context.Context, bucketID uuid.UUID, key, versionID string) ([]byte, error) {
	return nil, errors.ErrUnsupported
}

func (s *Store) UpdateObjectManifestRaw(ctx context.Context, bucketID uuid.UUID, key, versionID string, raw []byte) error {
	return errors.ErrUnsupported
}

func (s *Store) SetObjectReplicationStatus(ctx context.Context, bucketID uuid.UUID, key, versionID, status string) error {
	return errors.ErrUnsupported
}

func (s *Store) StartReshard(ctx context.Context, bucketID uuid.UUID, target int) (*meta.ReshardJob, error) {
	return nil, errors.ErrUnsupported
}

func (s *Store) GetReshardJob(ctx context.Context, bucketID uuid.UUID) (*meta.ReshardJob, error) {
	return nil, errors.ErrUnsupported
}

func (s *Store) UpdateReshardJob(ctx context.Context, job *meta.ReshardJob) error {
	return errors.ErrUnsupported
}

func (s *Store) CompleteReshard(ctx context.Context, bucketID uuid.UUID) error {
	return errors.ErrUnsupported
}

func (s *Store) ListReshardJobs(ctx context.Context) ([]*meta.ReshardJob, error) {
	return nil, errors.ErrUnsupported
}

// Compile-time guarantee that *Store satisfies meta.Store. Stories that
// touch the interface should preserve this assertion.
var _ meta.Store = (*Store)(nil)
