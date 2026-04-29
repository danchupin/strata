package tikv

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/meta"
)

// uuidFromString reuses uuid.Parse so JSON-stored BucketID strings round-trip
// to uuid.UUID without each caller importing the parser.
func uuidFromString(s string) (uuid.UUID, error) {
	return uuid.Parse(s)
}

// encodeBucket serialises a meta.Bucket into the byte slice we persist under
// BucketKey(name). JSON keeps the encoding human-readable for ops debugging
// and additive — old gateways decode rows written by newer ones with zero
// values for unknown fields.
func encodeBucket(b *meta.Bucket) ([]byte, error) {
	return json.Marshal(b)
}

// decodeBucket reverses encodeBucket.
func decodeBucket(raw []byte) (*meta.Bucket, error) {
	var b meta.Bucket
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, err
	}
	if b.Versioning == "" {
		b.Versioning = meta.VersioningDisabled
	}
	return &b, nil
}

// objectRow is the persisted shape for one object row. It mirrors meta.Object
// except Manifest is held as a raw blob — the data.{Encode,Decode}Manifest
// helpers route through the gateway's STRATA_MANIFEST_FORMAT toggle so new
// rows pick the active format and old rows decode regardless. The raw blob
// also lets the manifest rewriter (US-049) read/overwrite the bytes via the
// {Get,Update}ObjectManifestRaw methods without disturbing other fields.
type objectRow struct {
	BucketID          string            `json:"b"`
	Key               string            `json:"k"`
	VersionID         string            `json:"v"`
	IsLatest          bool              `json:"il,omitempty"`
	IsDeleteMarker    bool              `json:"dm,omitempty"`
	IsNull            bool              `json:"nu,omitempty"`
	Size              int64             `json:"sz,omitempty"`
	ETag              string            `json:"e,omitempty"`
	ContentType       string            `json:"ct,omitempty"`
	StorageClass      string            `json:"sc,omitempty"`
	Mtime             time.Time         `json:"mt"`
	ManifestRaw       []byte            `json:"m,omitempty"`
	UserMeta          map[string]string `json:"um,omitempty"`
	Tags              map[string]string `json:"tg,omitempty"`
	RetainUntil       time.Time         `json:"ru,omitempty"`
	RetainMode        string            `json:"rm,omitempty"`
	LegalHold         bool              `json:"lh,omitempty"`
	Checksums         map[string]string `json:"cs,omitempty"`
	SSE               string            `json:"sse,omitempty"`
	SSECKeyMD5        string            `json:"sk5,omitempty"`
	SSEKey            []byte            `json:"sk,omitempty"`
	SSEKeyID          string            `json:"ski,omitempty"`
	RestoreStatus     string            `json:"rs,omitempty"`
	PartsCount        int               `json:"pc,omitempty"`
	PartSizes         []int64           `json:"ps,omitempty"`
	CacheControl      string            `json:"cc,omitempty"`
	Expires           string            `json:"x,omitempty"`
	ReplicationStatus string            `json:"rps,omitempty"`
	ChecksumType      string            `json:"cty,omitempty"`
}

// encodeObject serialises a meta.Object into its TiKV row payload.
func encodeObject(o *meta.Object) ([]byte, error) {
	manifestBlob, err := data.EncodeManifest(o.Manifest)
	if err != nil {
		return nil, err
	}
	row := objectRow{
		BucketID:          o.BucketID.String(),
		Key:               o.Key,
		VersionID:         o.VersionID,
		IsLatest:          o.IsLatest,
		IsDeleteMarker:    o.IsDeleteMarker,
		IsNull:            o.IsNull,
		Size:              o.Size,
		ETag:              o.ETag,
		ContentType:       o.ContentType,
		StorageClass:      o.StorageClass,
		Mtime:             o.Mtime,
		ManifestRaw:       manifestBlob,
		UserMeta:          o.UserMeta,
		Tags:              o.Tags,
		RetainUntil:       o.RetainUntil,
		RetainMode:        o.RetainMode,
		LegalHold:         o.LegalHold,
		Checksums:         o.Checksums,
		SSE:               o.SSE,
		SSECKeyMD5:        o.SSECKeyMD5,
		SSEKey:            o.SSEKey,
		SSEKeyID:          o.SSEKeyID,
		RestoreStatus:     o.RestoreStatus,
		PartsCount:        o.PartsCount,
		PartSizes:         o.PartSizes,
		CacheControl:      o.CacheControl,
		Expires:           o.Expires,
		ReplicationStatus: o.ReplicationStatus,
		ChecksumType:      o.ChecksumType,
	}
	return json.Marshal(&row)
}

// decodeObject reverses encodeObject. The Manifest blob is decoded via
// data.DecodeManifest (which sniffs JSON-vs-proto by first byte) so rows
// written under either STRATA_MANIFEST_FORMAT round-trip transparently.
func decodeObject(raw []byte) (*meta.Object, error) {
	var row objectRow
	if err := json.Unmarshal(raw, &row); err != nil {
		return nil, err
	}
	manifest, err := data.DecodeManifest(row.ManifestRaw)
	if err != nil {
		return nil, err
	}
	bucketID, err := uuidFromString(row.BucketID)
	if err != nil {
		return nil, err
	}
	return &meta.Object{
		BucketID:          bucketID,
		Key:               row.Key,
		VersionID:         row.VersionID,
		IsLatest:          row.IsLatest,
		IsDeleteMarker:    row.IsDeleteMarker,
		IsNull:            row.IsNull,
		Size:              row.Size,
		ETag:              row.ETag,
		ContentType:       row.ContentType,
		StorageClass:      row.StorageClass,
		Mtime:             row.Mtime,
		Manifest:          manifest,
		UserMeta:          row.UserMeta,
		Tags:              row.Tags,
		RetainUntil:       row.RetainUntil,
		RetainMode:        row.RetainMode,
		LegalHold:         row.LegalHold,
		Checksums:         row.Checksums,
		SSE:               row.SSE,
		SSECKeyMD5:        row.SSECKeyMD5,
		SSEKey:            row.SSEKey,
		SSEKeyID:          row.SSEKeyID,
		RestoreStatus:     row.RestoreStatus,
		PartsCount:        row.PartsCount,
		PartSizes:         row.PartSizes,
		CacheControl:      row.CacheControl,
		Expires:           row.Expires,
		ReplicationStatus: row.ReplicationStatus,
		ChecksumType:      row.ChecksumType,
	}, nil
}
