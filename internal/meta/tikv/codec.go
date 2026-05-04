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

// multipartUploadRow is the persisted shape for one multipart_uploads row.
// Mirrors meta.MultipartUpload one-to-one. Short JSON keys keep the row
// payload tight (the encoding is internal — abbreviations are safe).
type multipartUploadRow struct {
	BucketID          string            `json:"b"`
	UploadID          string            `json:"u"`
	Key               string            `json:"k"`
	Status            string            `json:"st"`
	StorageClass      string            `json:"sc,omitempty"`
	ContentType       string            `json:"ct,omitempty"`
	InitiatedAt       time.Time         `json:"ia"`
	SSE               string            `json:"sse,omitempty"`
	SSEKey            []byte            `json:"sk,omitempty"`
	SSEKeyID          string            `json:"ski,omitempty"`
	UserMeta          map[string]string `json:"um,omitempty"`
	CacheControl      string            `json:"cc,omitempty"`
	Expires           string            `json:"x,omitempty"`
	ChecksumAlgorithm string            `json:"ca,omitempty"`
	ChecksumType      string            `json:"cty,omitempty"`
}

func encodeMultipart(mu *meta.MultipartUpload) ([]byte, error) {
	row := multipartUploadRow{
		BucketID:          mu.BucketID.String(),
		UploadID:          mu.UploadID,
		Key:               mu.Key,
		Status:            mu.Status,
		StorageClass:      mu.StorageClass,
		ContentType:       mu.ContentType,
		InitiatedAt:       mu.InitiatedAt,
		SSE:               mu.SSE,
		SSEKey:            mu.SSEKey,
		SSEKeyID:          mu.SSEKeyID,
		UserMeta:          mu.UserMeta,
		CacheControl:      mu.CacheControl,
		Expires:           mu.Expires,
		ChecksumAlgorithm: mu.ChecksumAlgorithm,
		ChecksumType:      mu.ChecksumType,
	}
	return json.Marshal(&row)
}

func decodeMultipart(raw []byte) (*meta.MultipartUpload, error) {
	var row multipartUploadRow
	if err := json.Unmarshal(raw, &row); err != nil {
		return nil, err
	}
	bucketID, err := uuidFromString(row.BucketID)
	if err != nil {
		return nil, err
	}
	return &meta.MultipartUpload{
		BucketID:          bucketID,
		UploadID:          row.UploadID,
		Key:               row.Key,
		Status:            row.Status,
		StorageClass:      row.StorageClass,
		ContentType:       row.ContentType,
		InitiatedAt:       row.InitiatedAt,
		SSE:               row.SSE,
		SSEKey:            row.SSEKey,
		SSEKeyID:          row.SSEKeyID,
		UserMeta:          row.UserMeta,
		CacheControl:      row.CacheControl,
		Expires:           row.Expires,
		ChecksumAlgorithm: row.ChecksumAlgorithm,
		ChecksumType:      row.ChecksumType,
	}, nil
}

// partRow is the persisted shape for one multipart_parts row. Manifest is
// held as a raw blob so STRATA_MANIFEST_FORMAT (proto vs JSON) flows through
// the same codec the object row uses.
type partRow struct {
	PartNumber  int               `json:"n"`
	ETag        string            `json:"e"`
	Size        int64             `json:"sz,omitempty"`
	Mtime       time.Time         `json:"mt"`
	ManifestRaw []byte            `json:"m,omitempty"`
	Checksums   map[string]string `json:"cs,omitempty"`
}

func encodePart(p *meta.MultipartPart) ([]byte, error) {
	manifestBlob, err := data.EncodeManifest(p.Manifest)
	if err != nil {
		return nil, err
	}
	row := partRow{
		PartNumber:  p.PartNumber,
		ETag:        p.ETag,
		Size:        p.Size,
		Mtime:       p.Mtime,
		ManifestRaw: manifestBlob,
		Checksums:   p.Checksums,
	}
	return json.Marshal(&row)
}

func decodePart(raw []byte) (*meta.MultipartPart, error) {
	var row partRow
	if err := json.Unmarshal(raw, &row); err != nil {
		return nil, err
	}
	manifest, err := data.DecodeManifest(row.ManifestRaw)
	if err != nil {
		return nil, err
	}
	return &meta.MultipartPart{
		PartNumber: row.PartNumber,
		ETag:       row.ETag,
		Size:       row.Size,
		Mtime:      row.Mtime,
		Manifest:   manifest,
		Checksums:  row.Checksums,
	}, nil
}

// multipartCompletionRow persists meta.MultipartCompletion alongside its
// ExpiresAt timestamp. TiKV has no native TTL; readers lazy-expire on Get.
type multipartCompletionRow struct {
	BucketID    string            `json:"b"`
	UploadID    string            `json:"u"`
	Key         string            `json:"k"`
	ETag        string            `json:"e"`
	VersionID   string            `json:"v,omitempty"`
	Body        []byte            `json:"bd,omitempty"`
	Headers     map[string]string `json:"h,omitempty"`
	CompletedAt time.Time         `json:"ca"`
	ExpiresAt   time.Time         `json:"x"`
}

func encodeMultipartCompletion(rec *meta.MultipartCompletion, expiresAt time.Time) ([]byte, error) {
	row := multipartCompletionRow{
		BucketID:    rec.BucketID.String(),
		UploadID:    rec.UploadID,
		Key:         rec.Key,
		ETag:        rec.ETag,
		VersionID:   rec.VersionID,
		Body:        rec.Body,
		Headers:     rec.Headers,
		CompletedAt: rec.CompletedAt,
		ExpiresAt:   expiresAt,
	}
	return json.Marshal(&row)
}

func decodeMultipartCompletion(raw []byte) (*meta.MultipartCompletion, time.Time, error) {
	var row multipartCompletionRow
	if err := json.Unmarshal(raw, &row); err != nil {
		return nil, time.Time{}, err
	}
	bucketID, err := uuidFromString(row.BucketID)
	if err != nil {
		return nil, time.Time{}, err
	}
	return &meta.MultipartCompletion{
		BucketID:    bucketID,
		UploadID:    row.UploadID,
		Key:         row.Key,
		ETag:        row.ETag,
		VersionID:   row.VersionID,
		Body:        row.Body,
		Headers:     row.Headers,
		CompletedAt: row.CompletedAt,
	}, row.ExpiresAt, nil
}

// auditRow is the persisted shape of one audit_log row. TiKV has no native
// TTL, so callers stamp ExpiresAt at write time and the sweeper
// (sweeper.go) eager-deletes after expiry; readers also lazy-skip expired
// rows so a missed sweep tick does not surface stale data.
type auditRow struct {
	BucketID    string    `json:"b"`
	Bucket      string    `json:"bn"`
	EventID     string    `json:"e"`
	Time        time.Time `json:"t"`
	Principal   string    `json:"p,omitempty"`
	Action      string    `json:"a,omitempty"`
	Resource    string    `json:"r,omitempty"`
	Result      string    `json:"rs,omitempty"`
	RequestID   string    `json:"rq,omitempty"`
	SourceIP    string    `json:"ip,omitempty"`
	UserAgent   string    `json:"ua,omitempty"`
	TotalTimeMS int       `json:"tt,omitempty"`
	ExpiresAt   time.Time `json:"x,omitempty"`
}

func encodeAudit(evt *meta.AuditEvent, expiresAt time.Time) ([]byte, error) {
	row := auditRow{
		BucketID:    evt.BucketID.String(),
		Bucket:      evt.Bucket,
		EventID:     evt.EventID,
		Time:        evt.Time,
		Principal:   evt.Principal,
		Action:      evt.Action,
		Resource:    evt.Resource,
		Result:      evt.Result,
		RequestID:   evt.RequestID,
		SourceIP:    evt.SourceIP,
		UserAgent:   evt.UserAgent,
		TotalTimeMS: evt.TotalTimeMS,
		ExpiresAt:   expiresAt,
	}
	return json.Marshal(&row)
}

func decodeAudit(raw []byte) (meta.AuditEvent, time.Time, error) {
	var row auditRow
	if err := json.Unmarshal(raw, &row); err != nil {
		return meta.AuditEvent{}, time.Time{}, err
	}
	bucketID, err := uuidFromString(row.BucketID)
	if err != nil {
		return meta.AuditEvent{}, time.Time{}, err
	}
	return meta.AuditEvent{
		BucketID:    bucketID,
		Bucket:      row.Bucket,
		EventID:     row.EventID,
		Time:        row.Time,
		Principal:   row.Principal,
		Action:      row.Action,
		Resource:    row.Resource,
		Result:      row.Result,
		RequestID:   row.RequestID,
		SourceIP:    row.SourceIP,
		UserAgent:   row.UserAgent,
		TotalTimeMS: row.TotalTimeMS,
	}, row.ExpiresAt, nil
}

// reshardJobRow is the persisted shape of one meta.ReshardJob. The
// global key (ReshardJobKey) carries the BucketID so we elide it from
// the body; we stamp it on decode from the key.
type reshardJobRow struct {
	Bucket    string    `json:"bn"`
	Source    int       `json:"s"`
	Target    int       `json:"t"`
	LastKey   string    `json:"lk,omitempty"`
	Done      bool      `json:"d,omitempty"`
	CreatedAt time.Time `json:"c"`
	UpdatedAt time.Time `json:"u"`
}

func encodeReshardJob(j *meta.ReshardJob) ([]byte, error) {
	row := reshardJobRow{
		Bucket:    j.Bucket,
		Source:    j.Source,
		Target:    j.Target,
		LastKey:   j.LastKey,
		Done:      j.Done,
		CreatedAt: j.CreatedAt,
		UpdatedAt: j.UpdatedAt,
	}
	return json.Marshal(&row)
}

func decodeReshardJob(bucketID uuid.UUID, raw []byte) (*meta.ReshardJob, error) {
	var row reshardJobRow
	if err := json.Unmarshal(raw, &row); err != nil {
		return nil, err
	}
	return &meta.ReshardJob{
		BucketID:  bucketID,
		Bucket:    row.Bucket,
		Source:    row.Source,
		Target:    row.Target,
		LastKey:   row.LastKey,
		Done:      row.Done,
		CreatedAt: row.CreatedAt,
		UpdatedAt: row.UpdatedAt,
	}, nil
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
