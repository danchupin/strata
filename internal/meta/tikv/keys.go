// Package tikv key encoding (US-002).
//
// TiKV is a flat byte-keyed KV store; this file is the single source of
// truth for how Strata's compound meta keys (bucket-id + shard + key +
// version-id, multipart upload IDs, audit partitions, queue cursors, …)
// are flattened onto it.
//
// See keys.md in this directory for the full layout, the rationale for
// each design choice, and worked examples.
//
// Two design rules govern every encoder:
//
//  1. Variable-length string segments are byte-stuffed FoundationDB-style
//     (0x00 → 0x00 0xFF) and terminated with 0x00 0x00. This preserves
//     lex ordering across heterogeneous lengths without length prefixes
//     (length prefixes would break range scans like "all keys starting
//     with prefix" because shorter keys would sort before longer ones
//     even when the longer key extends the shorter one in user space).
//
//  2. Object keys end with a 24-byte version-DESC suffix:
//     [MaxUint64 - timeuuid_ts_100ns]_8_BE || [raw uuid bytes]_16. The
//     inverted timestamp makes a forward-direction range scan emit the
//     latest version first; the raw 16 bytes survive the round-trip and
//     break ties when timestamps collide (rare; v1 timeuuids carry a
//     clockseq + node MAC that disambiguate).
package tikv

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	"github.com/google/uuid"
)

// Namespace is the single-byte (plus slash) top-level prefix Strata
// reserves on a TiKV cluster. Keeping the prefix tiny matters because
// every key carries it; "s/" is two bytes vs e.g. "strata/" at seven.
const Namespace = "s/"

// Top-level entity prefixes. Each picks a short discriminator byte so a
// sibling tenant on the same TiKV cluster can claim "s/" minus our
// reserved subset (or, more realistically, the operator picks a
// different Namespace for the sibling).
const (
	prefixBucketByName     = Namespace + "b/"   // s/b/<name>
	prefixBucketScoped     = Namespace + "B/"   // s/B/<uuid16>/...
	prefixIAMUser          = Namespace + "iu/"  // s/iu/<userName>
	prefixUserQuota        = Namespace + "uq/"  // s/uq/<userName>
	prefixIAMAccessKey     = Namespace + "ik/"  // s/ik/<accessKey>
	prefixIAMUserKeyIndex  = Namespace + "iuk/" // s/iuk/<userName>\x00\x00<accessKey>
	prefixManagedPolicy    = Namespace + "mp/"  // s/mp/<arn>
	prefixUserPolicy       = Namespace + "ups/" // s/ups/<userName>\x00\x00<policyArn>
	prefixPolicyUser       = Namespace + "pus/" // s/pus/<policyArn>\x00\x00<userName>
	prefixAccessPoint      = Namespace + "ap/"  // s/ap/<name>
	prefixAccessPointAlias = Namespace + "aa/"  // s/aa/<alias>
	prefixNotifyQueue      = Namespace + "qn/"  // s/qn/<bucket16><ts8><eventID>
	prefixNotifyDLQ        = Namespace + "qd/"  // s/qd/<bucket16><ts8><eventID>
	prefixReplicationQueue = Namespace + "qr/"  // s/qr/<bucket16><ts8><eventID>
	prefixAccessLogQueue   = Namespace + "qa/"  // s/qa/<bucket16><ts8><eventID>
	prefixGCQueue          = Namespace + "qg/"  // s/qg/<region>\x00\x00<ts8><oid>           (legacy; dual-write fallback during US-003 cutover)
	prefixGCQueueV2        = Namespace + "qG/"  // s/qG/<region>\x00\x00<shardID2BE><ts8><oid> (Phase 2 sharded; US-003)
	prefixAuditLog         = Namespace + "A/"   // s/A/<bucket16><day4><eventID>
	prefixLeaderLock       = Namespace + "L/"   // s/L/<lockName>
	prefixReshardJob       = Namespace + "Rj/"  // s/Rj/<bucket16>
	prefixAdminJob         = Namespace + "Aj/"  // s/Aj/<id>
	prefixHeartbeat        = Namespace + "hb/"  // s/hb/<nodeID>
	prefixClusterState     = Namespace + "cs/"  // s/cs/<clusterID>            (US-006 placement-rebalance drain sentinel)
)

// Bucket-scoped sub-prefixes. All are appended to a "s/B/<uuid16>/"
// header.
const (
	subBucketGrants     = "g"   // single key
	subObject           = "o/"  // s/B/<uuid16>/o/<escKey>\x00\x00<verDesc24>
	subObjectGrants     = "og/" // same shape as subObject
	subBucketBlob       = "c/"  // s/B/<uuid16>/c/<kind>      (kind is a fixed identifier, no escape)
	subBucketStats      = "bs"  // s/B/<uuid16>/bs            (single key, live counter)
	subUsageAgg         = "ua/" // s/B/<uuid16>/ua/<escClass>\x00\x00<day4>
	subUsageClassIndex  = "uc/" // s/B/<uuid16>/uc/<escClass>\x00\x00 (presence row)
	subInventoryConfig  = "i/"  // s/B/<uuid16>/i/<configID>
	subMultipart        = "u/"  // s/B/<uuid16>/u/<uploadID>
	subMultipartPart    = "up/" // s/B/<uuid16>/up/<uploadID>\x00\x00<partNum4>
	subMultipartCompl   = "mc/" // s/B/<uuid16>/mc/<uploadID>
	subReshardCursor    = "Rc/" // s/B/<uuid16>/Rc/<shard4> (per-shard progress; reserved, not yet exposed via meta.Store)
	subRewrapProgress   = "rw"  // single key
)

// BucketBlobKind enumerates the per-bucket single-document config blobs
// that every backend stores via the same set/get/delete blob helper
// pattern (CLAUDE.md, "blob-config helper"). Each kind has a fixed,
// short identifier — kept short because every blob key carries it.
const (
	BlobLifecycle          = "lc"
	BlobCORS               = "co"
	BlobPolicy             = "po"
	BlobPublicAccessBlock  = "pa"
	BlobOwnershipControls  = "oc"
	BlobEncryption         = "en"
	BlobNotification       = "no"
	BlobReplication        = "re"
	BlobWebsite            = "ws"
	BlobLogging            = "lg"
	BlobTagging            = "tg"
	BlobObjectLockConfig   = "ol"
	BlobQuota              = "qu"
	BlobPlacement          = "pl"
)

// versionDescLen is the length of the 24-byte version-DESC suffix
// (8B inverted ts + 16B raw uuid) appended to every object/object-grant
// key.
const versionDescLen = 24

// ----------------------------------------------------------------------------
// String segment encoding (byte-stuffing + 00 00 terminator).
// ----------------------------------------------------------------------------

// appendEscaped appends the byte-stuffed form of s to dst followed by
// the 00 00 terminator. The transform is:
//
//	0x00 → 0x00 0xFF
//	any other byte → itself
//	terminator → 0x00 0x00
//
// This is FoundationDB tuple-layer-style; see
// https://apple.github.io/foundationdb/data-modeling.html#tuples. It
// preserves lex ordering across heterogeneous-length segments: a
// shorter segment ends with 0x00 0x00 which is strictly less than any
// extension via 0x00 0xFF.
func appendEscaped(dst []byte, s string) []byte {
	for i := 0; i < len(s); i++ {
		b := s[i]
		if b == 0x00 {
			dst = append(dst, 0x00, 0xFF)
		} else {
			dst = append(dst, b)
		}
	}
	dst = append(dst, 0x00, 0x00)
	return dst
}

// readEscaped consumes one byte-stuffed + 00 00-terminated segment from
// the head of b and returns the decoded string and the remaining bytes.
// Returns an error if the segment is malformed or unterminated.
func readEscaped(b []byte) (string, []byte, error) {
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); {
		c := b[i]
		if c != 0x00 {
			out = append(out, c)
			i++
			continue
		}
		// Saw 0x00 — must be followed by 0x00 (terminator) or 0xFF (escape).
		if i+1 >= len(b) {
			return "", nil, errors.New("tikv keys: unterminated escaped segment")
		}
		switch b[i+1] {
		case 0x00:
			return string(out), b[i+2:], nil
		case 0xFF:
			out = append(out, 0x00)
			i += 2
		default:
			return "", nil, fmt.Errorf("tikv keys: invalid escape 0x00 0x%02x", b[i+1])
		}
	}
	return "", nil, errors.New("tikv keys: unterminated escaped segment")
}

// ----------------------------------------------------------------------------
// Version-DESC suffix.
// ----------------------------------------------------------------------------

// EncodeVersionDesc returns the 24-byte segment that lex-sorts in
// version-DESC order (latest first). The high 8 bytes are MaxUint64 -
// timeuuid_ts_100ns big-endian; the low 16 bytes are the raw UUID bytes
// for tie-break and round-trip recovery.
//
// The null sentinel UUID (NullVersionID, all zeros) has timestamp 0 and
// thus encodes as MaxUint64 in the high 8 bytes — it sorts last among
// the versions of a given object key. The gateway resolves
// "?versionId=null" by exact lookup (not by scan) so the position is
// observable only to internal range-scan code, which is not sensitive
// to it.
func EncodeVersionDesc(versionID string) ([]byte, error) {
	id, err := uuid.Parse(versionID)
	if err != nil {
		return nil, fmt.Errorf("tikv keys: parse version id %q: %w", versionID, err)
	}
	out := make([]byte, versionDescLen)
	ts := uint64(id.Time())
	binary.BigEndian.PutUint64(out[:8], math.MaxUint64-ts)
	copy(out[8:], id[:])
	return out, nil
}

// DecodeVersionDesc returns the version-id string carried by the
// 24-byte suffix produced by EncodeVersionDesc. The inverted-ts half is
// not needed for recovery (the raw UUID bytes carry it) but is asserted
// to match the UUID's own Time() so corrupt suffixes fail loudly.
func DecodeVersionDesc(b []byte) (string, error) {
	if len(b) != versionDescLen {
		return "", fmt.Errorf("tikv keys: version desc must be %d bytes, got %d", versionDescLen, len(b))
	}
	var id uuid.UUID
	copy(id[:], b[8:])
	if got, want := binary.BigEndian.Uint64(b[:8]), math.MaxUint64-uint64(id.Time()); got != want {
		return "", fmt.Errorf("tikv keys: version desc inverted-ts %#016x mismatches uuid time inverse %#016x", got, want)
	}
	return id.String(), nil
}

// ----------------------------------------------------------------------------
// Bucket and bucket-scoped keys.
// ----------------------------------------------------------------------------

// BucketKey returns the lookup key for the bucket row addressed by
// name. Buckets are unique by name globally; the row stores the bucket
// UUID, owner, versioning state, etc.
func BucketKey(name string) []byte {
	out := []byte(prefixBucketByName)
	return appendEscaped(out, name)
}

// PrefixForBucket returns "s/B/<uuid16>/" — the start of every
// bucket-scoped key for bucketID. Useful as an emptiness probe in
// DeleteBucket (range scan with limit 1) and as an upper-bound builder
// (append a single 0xFF byte to scan one bucket exclusively).
func PrefixForBucket(bucketID uuid.UUID) []byte {
	out := make([]byte, 0, len(prefixBucketScoped)+16+1)
	out = append(out, prefixBucketScoped...)
	out = append(out, bucketID[:]...)
	out = append(out, '/')
	return out
}

// ObjectPrefix returns "s/B/<uuid16>/o/" — the start of every object
// key in bucketID. Range scans for ListObjects/ListObjectVersions
// originate here.
func ObjectPrefix(bucketID uuid.UUID) []byte {
	return append(PrefixForBucket(bucketID), subObject...)
}

// ObjectPrefixWithKey returns "s/B/<uuid16>/o/<escKey-first-bytes>" —
// suitable for narrowing a scan to keys lex-≥ keyPrefix in the object
// namespace. The returned bytes intentionally do NOT carry a 00 00
// terminator (we want every key whose escaped form starts with the
// prefix, not just keys equal to keyPrefix).
func ObjectPrefixWithKey(bucketID uuid.UUID, keyPrefix string) []byte {
	out := ObjectPrefix(bucketID)
	for i := 0; i < len(keyPrefix); i++ {
		b := keyPrefix[i]
		if b == 0x00 {
			out = append(out, 0x00, 0xFF)
		} else {
			out = append(out, b)
		}
	}
	return out
}

// ObjectKey returns the full object key for (bucketID, key, versionID).
// Encoding: s/B/<uuid16>/o/<escKey>\x00\x00<verDesc24>. Range scans
// over a single user-space key see the latest version first because
// the version-desc suffix lex-sorts ascending = ts-descending.
func ObjectKey(bucketID uuid.UUID, key, versionID string) ([]byte, error) {
	suffix, err := EncodeVersionDesc(versionID)
	if err != nil {
		return nil, err
	}
	out := ObjectPrefix(bucketID)
	out = appendEscaped(out, key)
	out = append(out, suffix...)
	return out, nil
}

// DecodeObjectKey reverses ObjectKey. Returns the bucket UUID, user-
// space key, and version-id string.
func DecodeObjectKey(k []byte) (uuid.UUID, string, string, error) {
	prefix := []byte(prefixBucketScoped)
	if len(k) < len(prefix)+16+1+len(subObject)+versionDescLen {
		return uuid.UUID{}, "", "", errors.New("tikv keys: object key too short")
	}
	if string(k[:len(prefix)]) != prefixBucketScoped {
		return uuid.UUID{}, "", "", errors.New("tikv keys: object key missing scoped prefix")
	}
	rest := k[len(prefix):]
	var id uuid.UUID
	copy(id[:], rest[:16])
	rest = rest[16:]
	if rest[0] != '/' {
		return uuid.UUID{}, "", "", errors.New("tikv keys: object key missing slash after bucket")
	}
	rest = rest[1:]
	if string(rest[:len(subObject)]) != subObject {
		return uuid.UUID{}, "", "", errors.New("tikv keys: object key missing object subprefix")
	}
	rest = rest[len(subObject):]
	if len(rest) < versionDescLen {
		return uuid.UUID{}, "", "", errors.New("tikv keys: object key truncated before version-desc")
	}
	keyEsc := rest[:len(rest)-versionDescLen]
	verBytes := rest[len(rest)-versionDescLen:]
	keyStr, tail, err := readEscaped(keyEsc)
	if err != nil {
		return uuid.UUID{}, "", "", fmt.Errorf("tikv keys: object key body: %w", err)
	}
	if len(tail) != 0 {
		return uuid.UUID{}, "", "", errors.New("tikv keys: trailing bytes after object key body")
	}
	verStr, err := DecodeVersionDesc(verBytes)
	if err != nil {
		return uuid.UUID{}, "", "", err
	}
	return id, keyStr, verStr, nil
}

// ObjectGrantsKey mirrors ObjectKey under the "og/" sub-prefix.
func ObjectGrantsKey(bucketID uuid.UUID, key, versionID string) ([]byte, error) {
	suffix, err := EncodeVersionDesc(versionID)
	if err != nil {
		return nil, err
	}
	out := append(PrefixForBucket(bucketID), subObjectGrants...)
	out = appendEscaped(out, key)
	out = append(out, suffix...)
	return out, nil
}

// BucketGrantsKey is the single-row key for a bucket's persisted ACL
// grants.
func BucketGrantsKey(bucketID uuid.UUID) []byte {
	return append(PrefixForBucket(bucketID), subBucketGrants...)
}

// BucketBlobKey is the per-bucket single-document blob slot for the
// given config kind. Reuse the BlobX constants — never pass a free-form
// kind string from a handler.
func BucketBlobKey(bucketID uuid.UUID, kind string) []byte {
	out := append(PrefixForBucket(bucketID), subBucketBlob...)
	return append(out, kind...)
}

// BucketStatsKey is the single-row live counter slot for a bucket
// (US-004..US-005). Bumped via a pessimistic txn under
// internal/meta/tikv/store.go::BumpBucketStats.
func BucketStatsKey(bucketID uuid.UUID) []byte {
	return append(PrefixForBucket(bucketID), subBucketStats...)
}

// UsageAggregateKey is the per-(bucket, storageClass, day) usage rollup row
// (US-008). Day is encoded as a 4-byte big-endian Unix-epoch-day so range
// scans by day return ascending.
func UsageAggregateKey(bucketID uuid.UUID, storageClass string, dayEpoch uint32) []byte {
	out := append(PrefixForBucket(bucketID), subUsageAgg...)
	out = appendEscaped(out, storageClass)
	var d [4]byte
	binary.BigEndian.PutUint32(d[:], dayEpoch)
	return append(out, d[:]...)
}

// UsageAggregateClassPrefix returns the start of usage rollup rows for a
// single (bucket, storageClass). Range scans bracket [start, prefixEnd(start))
// to read every day in that class.
func UsageAggregateClassPrefix(bucketID uuid.UUID, storageClass string) []byte {
	out := append(PrefixForBucket(bucketID), subUsageAgg...)
	return appendEscaped(out, storageClass)
}

// UsageClassIndexKey is the presence row written next to every usage
// aggregate so ListUserUsage can enumerate the storage classes a bucket
// has rolled up without a global scan.
func UsageClassIndexKey(bucketID uuid.UUID, storageClass string) []byte {
	out := append(PrefixForBucket(bucketID), subUsageClassIndex...)
	return appendEscaped(out, storageClass)
}

// UsageClassIndexPrefix is the per-bucket presence-row scan origin.
func UsageClassIndexPrefix(bucketID uuid.UUID) []byte {
	return append(PrefixForBucket(bucketID), subUsageClassIndex...)
}

// InventoryConfigKey is one row per (bucket, configID).
func InventoryConfigKey(bucketID uuid.UUID, configID string) []byte {
	out := append(PrefixForBucket(bucketID), subInventoryConfig...)
	return appendEscaped(out, configID)
}

// InventoryConfigPrefix is the start of all inventory configs for a
// bucket — used by ListBucketInventoryConfigs as a range-scan origin.
func InventoryConfigPrefix(bucketID uuid.UUID) []byte {
	return append(PrefixForBucket(bucketID), subInventoryConfig...)
}

// MultipartKey is the single-row key for the multipart upload status
// row.
func MultipartKey(bucketID uuid.UUID, uploadID string) []byte {
	out := append(PrefixForBucket(bucketID), subMultipart...)
	return appendEscaped(out, uploadID)
}

// MultipartPrefix is the start of every multipart upload status row in
// the bucket — origin for ListMultipartUploads.
func MultipartPrefix(bucketID uuid.UUID) []byte {
	return append(PrefixForBucket(bucketID), subMultipart...)
}

// MultipartPartKey is one row per (bucket, uploadID, partNumber).
// PartNumber is encoded big-endian uint32 so range scans return parts
// in ascending number order.
func MultipartPartKey(bucketID uuid.UUID, uploadID string, partNumber int) []byte {
	out := append(PrefixForBucket(bucketID), subMultipartPart...)
	out = appendEscaped(out, uploadID)
	var pn [4]byte
	binary.BigEndian.PutUint32(pn[:], uint32(partNumber))
	return append(out, pn[:]...)
}

// MultipartPartPrefix is the start of all part rows for a single
// (bucket, uploadID) — origin for ListParts.
func MultipartPartPrefix(bucketID uuid.UUID, uploadID string) []byte {
	out := append(PrefixForBucket(bucketID), subMultipartPart...)
	return appendEscaped(out, uploadID)
}

// MultipartCompletionKey is the idempotency record for a successful
// CompleteMultipartUpload (used to replay a retried Complete).
func MultipartCompletionKey(bucketID uuid.UUID, uploadID string) []byte {
	out := append(PrefixForBucket(bucketID), subMultipartCompl...)
	return appendEscaped(out, uploadID)
}

// ReshardJobKey is the single-row key for the active or queued reshard
// job for a bucket. Lives under a global prefix (rather than the
// bucket-scoped one) so ListReshardJobs is a single ordered range scan
// across every bucket without touching unrelated bucket-scoped rows —
// mirrors the Cassandra reshard_jobs table partitioning shape.
func ReshardJobKey(bucketID uuid.UUID) []byte {
	out := make([]byte, 0, len(prefixReshardJob)+16)
	out = append(out, prefixReshardJob...)
	return append(out, bucketID[:]...)
}

// ReshardJobsPrefix is the global scan origin for ListReshardJobs.
func ReshardJobsPrefix() []byte {
	return []byte(prefixReshardJob)
}

// AdminJobKey is the per-id row for an admin background job (US-002).
// IDs are server-minted UUIDs but escaped + terminated to keep the same
// shape as other variable-length-segment encodings in this file.
func AdminJobKey(id string) []byte {
	return appendEscaped([]byte(prefixAdminJob), id)
}

// ReshardCursorKey is one row per (bucket, shardID) tracking the
// per-shard progress watermark of an in-progress reshard.
func ReshardCursorKey(bucketID uuid.UUID, shardID int) []byte {
	out := append(PrefixForBucket(bucketID), subReshardCursor...)
	var sh [4]byte
	binary.BigEndian.PutUint32(sh[:], uint32(shardID))
	return append(out, sh[:]...)
}

// RewrapProgressKey is the single-row key for the SSE master-key
// rewrap watermark for a bucket.
func RewrapProgressKey(bucketID uuid.UUID) []byte {
	return append(PrefixForBucket(bucketID), subRewrapProgress...)
}

// ----------------------------------------------------------------------------
// IAM keys.
// ----------------------------------------------------------------------------

// IAMUserKey is the per-user record key.
func IAMUserKey(userName string) []byte {
	return appendEscaped([]byte(prefixIAMUser), userName)
}

// UserQuotaKey is the per-user quota record key (US-003).
func UserQuotaKey(userName string) []byte {
	return appendEscaped([]byte(prefixUserQuota), userName)
}

// IAMUserPrefix is the start of all IAM user rows — origin for
// ListIAMUsers.
func IAMUserPrefix() []byte {
	return []byte(prefixIAMUser)
}

// IAMAccessKeyKey is the per-access-key record key. SigV4 verification
// looks this up on every request — the encoding is therefore a single
// Get with no scan.
func IAMAccessKeyKey(accessKeyID string) []byte {
	return appendEscaped([]byte(prefixIAMAccessKey), accessKeyID)
}

// IAMUserAccessKeyKey is the (userName, accessKeyID) index row used by
// ListIAMAccessKeys. The userName segment is escaped + terminated so
// per-user range scans are clean.
func IAMUserAccessKeyKey(userName, accessKeyID string) []byte {
	out := []byte(prefixIAMUserKeyIndex)
	out = appendEscaped(out, userName)
	return appendEscaped(out, accessKeyID)
}

// IAMUserAccessKeyPrefix is the start of all access-key index rows for
// userName — origin for the per-user range scan that backs
// ListIAMAccessKeys.
func IAMUserAccessKeyPrefix(userName string) []byte {
	out := []byte(prefixIAMUserKeyIndex)
	return appendEscaped(out, userName)
}

// ManagedPolicyKey is the per-policy record key (lookup by ARN). Mirrors
// IAMUserKey shape — global single-document slot.
func ManagedPolicyKey(arn string) []byte {
	return appendEscaped([]byte(prefixManagedPolicy), arn)
}

// ManagedPolicyPrefix is the start of all managed-policy rows — origin for
// ListManagedPolicies.
func ManagedPolicyPrefix() []byte {
	return []byte(prefixManagedPolicy)
}

// UserPolicyKey is the per-attachment row keyed (userName, policyArn). The
// userName segment is escaped + terminated so per-user range scans
// (ListUserPolicies) emit attachments cleanly.
func UserPolicyKey(userName, policyArn string) []byte {
	out := []byte(prefixUserPolicy)
	out = appendEscaped(out, userName)
	return appendEscaped(out, policyArn)
}

// UserPolicyPrefix is the start of all attachment rows for userName.
func UserPolicyPrefix(userName string) []byte {
	out := []byte(prefixUserPolicy)
	return appendEscaped(out, userName)
}

// PolicyUserKey is the inverse-index row keyed (policyArn, userName) used by
// DeleteManagedPolicy to detect attachments without a global scan.
func PolicyUserKey(policyArn, userName string) []byte {
	out := []byte(prefixPolicyUser)
	out = appendEscaped(out, policyArn)
	return appendEscaped(out, userName)
}

// PolicyUserPrefix is the start of all inverse-index rows for policyArn.
func PolicyUserPrefix(policyArn string) []byte {
	out := []byte(prefixPolicyUser)
	return appendEscaped(out, policyArn)
}

// ----------------------------------------------------------------------------
// Access points.
// ----------------------------------------------------------------------------

// AccessPointKey is the per-access-point record key (lookup by name).
func AccessPointKey(name string) []byte {
	return appendEscaped([]byte(prefixAccessPoint), name)
}

// AccessPointAliasKey is the alias → name index row.
func AccessPointAliasKey(alias string) []byte {
	return appendEscaped([]byte(prefixAccessPointAlias), alias)
}

// AccessPointPrefix is the start of all access-point rows — origin for
// the global ListAccessPoints fan-out (uuid.Nil filter).
func AccessPointPrefix() []byte {
	return []byte(prefixAccessPoint)
}

// ----------------------------------------------------------------------------
// Queues + audit log.
// ----------------------------------------------------------------------------

// queueKey is the shared shape for per-bucket FIFO queues:
//
//	<prefix><bucket16><tsNano8-BE><eventID-escaped>
//
// Big-endian ts8 means a forward range scan claims oldest first
// (ts ascending); event-id is appended for tiebreak when two events
// land in the same nanosecond.
func queueKey(prefix string, bucketID uuid.UUID, tsNano uint64, eventID string) []byte {
	out := make([]byte, 0, len(prefix)+16+8+len(eventID)+2)
	out = append(out, prefix...)
	out = append(out, bucketID[:]...)
	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], tsNano)
	out = append(out, ts[:]...)
	return appendEscaped(out, eventID)
}

func queuePrefix(prefix string, bucketID uuid.UUID) []byte {
	out := make([]byte, 0, len(prefix)+16)
	out = append(out, prefix...)
	return append(out, bucketID[:]...)
}

// NotifyQueueKey enqueues an S3-event-notification waiting for the
// notify worker.
func NotifyQueueKey(bucketID uuid.UUID, tsNano uint64, eventID string) []byte {
	return queueKey(prefixNotifyQueue, bucketID, tsNano, eventID)
}

// NotifyQueuePrefix is the per-bucket origin for claim scans.
func NotifyQueuePrefix(bucketID uuid.UUID) []byte {
	return queuePrefix(prefixNotifyQueue, bucketID)
}

// NotifyDLQKey is the dead-letter slot for a notification that
// exhausted retries.
func NotifyDLQKey(bucketID uuid.UUID, tsNano uint64, eventID string) []byte {
	return queueKey(prefixNotifyDLQ, bucketID, tsNano, eventID)
}

// NotifyDLQPrefix is the per-bucket DLQ scan origin.
func NotifyDLQPrefix(bucketID uuid.UUID) []byte {
	return queuePrefix(prefixNotifyDLQ, bucketID)
}

// ReplicationQueueKey enqueues a cross-region replication intent.
func ReplicationQueueKey(bucketID uuid.UUID, tsNano uint64, eventID string) []byte {
	return queueKey(prefixReplicationQueue, bucketID, tsNano, eventID)
}

// ReplicationQueuePrefix is the per-bucket replication scan origin.
func ReplicationQueuePrefix(bucketID uuid.UUID) []byte {
	return queuePrefix(prefixReplicationQueue, bucketID)
}

// AccessLogQueueKey buffers one access-log row pending flush.
func AccessLogQueueKey(bucketID uuid.UUID, tsNano uint64, eventID string) []byte {
	return queueKey(prefixAccessLogQueue, bucketID, tsNano, eventID)
}

// AccessLogQueuePrefix is the per-bucket access-log scan origin.
func AccessLogQueuePrefix(bucketID uuid.UUID) []byte {
	return queuePrefix(prefixAccessLogQueue, bucketID)
}

// GCQueueKey enqueues a chunk-deletion waiting for the GC worker.
// Region is escaped (it is operator-controlled, not a fixed identifier
// like bucket-blob kinds) so colons and slashes are safe.
func GCQueueKey(region string, tsNano uint64, oid string) []byte {
	out := []byte(prefixGCQueue)
	out = appendEscaped(out, region)
	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], tsNano)
	out = append(out, ts[:]...)
	return appendEscaped(out, oid)
}

// GCQueuePrefix is the per-region GC scan origin.
func GCQueuePrefix(region string) []byte {
	out := []byte(prefixGCQueue)
	return appendEscaped(out, region)
}

// GCQueueKeyV2 is the Phase 2 (US-003) sharded key shape: the 1024-wide
// logical-shard id is encoded as a fixed 2-byte big-endian segment between
// the region terminator and the timestamp. A `shardCount`-wide runtime
// reader scans only the prefixes its modulo class owns, dodging the legacy
// region-wide fan-out that single-leader gc saturated. shardID range
// [0, meta.GCShardCount) — caller-enforced.
func GCQueueKeyV2(region string, shardID uint16, tsNano uint64, oid string) []byte {
	out := []byte(prefixGCQueueV2)
	out = appendEscaped(out, region)
	var sid [2]byte
	binary.BigEndian.PutUint16(sid[:], shardID)
	out = append(out, sid[:]...)
	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], tsNano)
	out = append(out, ts[:]...)
	return appendEscaped(out, oid)
}

// GCQueueShardPrefixV2 is the per-(region, shardID) v2 scan origin. A range
// scan over [prefix, prefixEnd(prefix)) returns every entry of one logical
// shard in ts-ascending order.
func GCQueueShardPrefixV2(region string, shardID uint16) []byte {
	out := []byte(prefixGCQueueV2)
	out = appendEscaped(out, region)
	var sid [2]byte
	binary.BigEndian.PutUint16(sid[:], shardID)
	return append(out, sid[:]...)
}

// GCQueueRegionPrefixV2 is the per-region v2 scan origin (covers every
// logical shard). Intended for diagnostics / drains where the caller wants
// the whole region irrespective of shard mapping.
func GCQueueRegionPrefixV2(region string) []byte {
	out := []byte(prefixGCQueueV2)
	return appendEscaped(out, region)
}

// AuditLogKey is a single audit row keyed by (bucket, day, eventID).
// Day is the UTC day-epoch as uint32 BE (days since 1970-01-01) so
// per-bucket per-day partitions are recoverable from the key — the
// audit-export worker (US-046) walks partitions older than the
// retention window via this layout.
func AuditLogKey(bucketID uuid.UUID, dayEpoch uint32, eventID string) []byte {
	out := make([]byte, 0, len(prefixAuditLog)+16+4+len(eventID)+2)
	out = append(out, prefixAuditLog...)
	out = append(out, bucketID[:]...)
	var d [4]byte
	binary.BigEndian.PutUint32(d[:], dayEpoch)
	out = append(out, d[:]...)
	return appendEscaped(out, eventID)
}

// AuditLogDayPrefix is the (bucket, day) partition scan origin used by
// ReadAuditPartition / DeleteAuditPartition.
func AuditLogDayPrefix(bucketID uuid.UUID, dayEpoch uint32) []byte {
	out := make([]byte, 0, len(prefixAuditLog)+16+4)
	out = append(out, prefixAuditLog...)
	out = append(out, bucketID[:]...)
	var d [4]byte
	binary.BigEndian.PutUint32(d[:], dayEpoch)
	return append(out, d[:]...)
}

// AuditLogBucketPrefix is the per-bucket audit scan origin (all days).
func AuditLogBucketPrefix(bucketID uuid.UUID) []byte {
	out := make([]byte, 0, len(prefixAuditLog)+16)
	out = append(out, prefixAuditLog...)
	return append(out, bucketID[:]...)
}

// ----------------------------------------------------------------------------
// Leader lock.
// ----------------------------------------------------------------------------

// LeaderLockKey is the lease row used by internal/meta/tikv.Locker
// (US-011). Names like "gc-leader", "lifecycle-leader",
// "audit-sweeper-leader-tikv".
func LeaderLockKey(name string) []byte {
	return appendEscaped([]byte(prefixLeaderLock), name)
}

// ClusterStateKey is the per-cluster drain-state row (US-006). NOT
// bucket-scoped — clusterIDs live in a global namespace.
func ClusterStateKey(clusterID string) []byte {
	return appendEscaped([]byte(prefixClusterState), clusterID)
}

// ClusterStatePrefix is the global scan origin for ListClusterStates.
func ClusterStatePrefix() []byte {
	return []byte(prefixClusterState)
}
