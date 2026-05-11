// Package inventory ships the strata-inventory worker: a leader-elected
// background worker that ticks per bucket InventoryConfiguration, walks the
// source bucket, and writes a manifest.json + CSV.gz pair into the configured
// target bucket. CSV columns match the AWS Inventory spec.
package inventory

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/md5"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/meta"
	strataotel "github.com/danchupin/strata/internal/otel"
)

// Frequencies the AWS InventoryConfiguration spec accepts. A worker tick fires
// at most once per period per (bucketID, configID) — the period boundary is
// floor(now / unit) so two ticks in the same hour do not produce two reports.
const (
	frequencyHourly = "hourly"
	frequencyDaily  = "daily"
	frequencyWeekly = "weekly"
)

// Config wires a Worker. Defaults applied in New: Interval=5m, Now=time.Now,
// Logger=slog.Default. Region propagates to data.Backend.PutChunks for the
// target bucket; empty falls back to "default" matching the s3api server.
type Config struct {
	Meta     meta.Store
	Data     data.Backend
	Logger   *slog.Logger
	Interval time.Duration
	Now      func() time.Time
	Region   string
	// Tracer emits per-iteration parent spans (`worker.inventory.tick`) plus
	// `inventory.scan_bucket` sub-op children. Nil falls back to a process-
	// shared no-op tracer.
	Tracer trace.Tracer
}

// Worker drains every bucket's inventory configurations on each tick and
// produces (manifest.json, CSV.gz) pairs into target buckets when a config is
// due. The lastRun map is keyed by (bucketID, configID) → start-of-period and
// keeps the worker from re-emitting a report inside the same period.
type Worker struct {
	cfg Config

	mu      sync.Mutex
	lastRun map[runKey]time.Time

	iterErrMu sync.Mutex
	iterErr   error
}

func (w *Worker) tracerOrNoop() trace.Tracer {
	if w.cfg.Tracer == nil {
		return strataotel.NoopTracer()
	}
	return w.cfg.Tracer
}

func (w *Worker) recordIterErr(err error) {
	if err == nil {
		return
	}
	w.iterErrMu.Lock()
	if w.iterErr == nil {
		w.iterErr = err
	}
	w.iterErrMu.Unlock()
}

func (w *Worker) takeIterErr() error {
	w.iterErrMu.Lock()
	defer w.iterErrMu.Unlock()
	err := w.iterErr
	w.iterErr = nil
	return err
}

type runKey struct {
	BucketID uuid.UUID
	ConfigID string
}

// inventoryXML mirrors the parse-only fields the worker needs from the bucket
// blob. Round-tripping the full schema lives in s3api/inventory.go; here we
// only pluck the operational fields.
type inventoryXML struct {
	XMLName     xml.Name `xml:"InventoryConfiguration"`
	ID          string   `xml:"Id"`
	IsEnabled   bool     `xml:"IsEnabled"`
	Destination *struct {
		S3BucketDestination *struct {
			Bucket    string `xml:"Bucket"`
			Format    string `xml:"Format"`
			Prefix    string `xml:"Prefix"`
			AccountID string `xml:"AccountId"`
		} `xml:"S3BucketDestination"`
	} `xml:"Destination"`
	Schedule *struct {
		Frequency string `xml:"Frequency"`
	} `xml:"Schedule"`
	IncludedObjectVersions string `xml:"IncludedObjectVersions"`
	Filter                 *struct {
		Prefix string `xml:"Prefix"`
	} `xml:"Filter,omitempty"`
}

// CSVHeader is the AWS-spec column list the worker emits as the first CSV row.
// Tests assert on this exact header so consumers can pin a parser shape.
var CSVHeader = []string{
	"Bucket",
	"Key",
	"VersionId",
	"IsLatest",
	"IsDeleteMarker",
	"Size",
	"LastModifiedDate",
	"ETag",
	"StorageClass",
	"ChecksumAlgorithm",
}

func New(cfg Config) (*Worker, error) {
	if cfg.Meta == nil {
		return nil, errors.New("inventory: meta store required")
	}
	if cfg.Data == nil {
		return nil, errors.New("inventory: data backend required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 5 * time.Minute
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Region == "" {
		cfg.Region = "default"
	}
	return &Worker{
		cfg:     cfg,
		lastRun: make(map[runKey]time.Time),
	}, nil
}

// Run loops on cfg.Interval until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) error {
	w.cfg.Logger.Info("inventory: starting", "interval", w.cfg.Interval.String())
	t := time.NewTicker(w.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if err := w.RunOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
				w.cfg.Logger.Warn("inventory: tick failed", "error", err.Error())
			}
		}
	}
}

// RunOnce performs a single inventory pass over every bucket.
func (w *Worker) RunOnce(ctx context.Context) error {
	iterCtx, span := strataotel.StartIteration(ctx, w.tracerOrNoop(), "inventory")
	err := w.runOnce(iterCtx)
	if err == nil {
		err = w.takeIterErr()
	} else {
		_ = w.takeIterErr()
	}
	strataotel.EndIteration(span, err)
	return err
}

func (w *Worker) runOnce(ctx context.Context) error {
	buckets, err := w.cfg.Meta.ListBuckets(ctx, "")
	if err != nil {
		return fmt.Errorf("list buckets: %w", err)
	}
	for _, b := range buckets {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		bucketCtx, bucketSpan := w.tracerOrNoop().Start(ctx, "inventory.scan_bucket",
			trace.WithSpanKind(trace.SpanKindInternal),
			trace.WithAttributes(
				strataotel.AttrComponentWorker,
				attribute.String(strataotel.WorkerKey, "inventory"),
				attribute.String("strata.inventory.bucket", b.Name),
				attribute.String("strata.inventory.bucket_id", b.ID.String()),
			),
		)
		w.scanBucket(bucketCtx, b, bucketSpan)
		bucketSpan.End()
	}
	return nil
}

func (w *Worker) scanBucket(ctx context.Context, b *meta.Bucket, span trace.Span) {
	configs, err := w.cfg.Meta.ListBucketInventoryConfigs(ctx, b.ID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		w.recordIterErr(err)
		w.cfg.Logger.Warn("inventory: list configs", "bucket", b.Name, "error", err.Error())
		return
	}
	for id, blob := range configs {
		if ctx.Err() != nil {
			return
		}
		cfg, perr := parseInventoryXML(blob)
		if perr != nil {
			w.cfg.Logger.Warn("inventory: parse config", "bucket", b.Name, "id", id, "error", perr.Error())
			continue
		}
		if !cfg.IsEnabled {
			continue
		}
		if !w.due(b.ID, id, cfg) {
			continue
		}
		if rerr := w.runConfig(ctx, b, id, cfg); rerr != nil {
			span.RecordError(rerr)
			span.SetStatus(codes.Error, rerr.Error())
			w.recordIterErr(rerr)
			w.cfg.Logger.Warn("inventory: run config", "bucket", b.Name, "id", id, "error", rerr.Error())
			continue
		}
	}
}

func parseInventoryXML(blob []byte) (*inventoryXML, error) {
	var c inventoryXML
	if err := xml.Unmarshal(blob, &c); err != nil {
		return nil, err
	}
	if c.Destination == nil || c.Destination.S3BucketDestination == nil {
		return nil, errors.New("missing destination")
	}
	if strings.TrimSpace(c.Destination.S3BucketDestination.Bucket) == "" {
		return nil, errors.New("missing destination bucket")
	}
	if c.Schedule == nil || c.Schedule.Frequency == "" {
		return nil, errors.New("missing schedule")
	}
	return &c, nil
}

// due returns true when (bucketID, configID) has not produced a report inside
// the current period defined by cfg.Schedule.Frequency.
func (w *Worker) due(bucketID uuid.UUID, configID string, cfg *inventoryXML) bool {
	period, ok := periodFor(cfg.Schedule.Frequency)
	if !ok {
		return false
	}
	key := runKey{BucketID: bucketID, ConfigID: configID}
	now := w.cfg.Now()
	periodStart := now.Truncate(period)
	w.mu.Lock()
	defer w.mu.Unlock()
	last, seen := w.lastRun[key]
	if seen && !last.Before(periodStart) {
		return false
	}
	w.lastRun[key] = periodStart
	return true
}

func periodFor(freq string) (time.Duration, bool) {
	switch strings.ToLower(freq) {
	case frequencyHourly:
		return time.Hour, true
	case frequencyDaily:
		return 24 * time.Hour, true
	case frequencyWeekly:
		return 7 * 24 * time.Hour, true
	}
	return 0, false
}

// runConfig walks the source bucket, builds the CSV.gz + manifest.json, and
// writes both into the target bucket via meta.Store + data.Backend.
func (w *Worker) runConfig(ctx context.Context, src *meta.Bucket, configID string, cfg *inventoryXML) error {
	targetBucketName := stripBucketARN(cfg.Destination.S3BucketDestination.Bucket)
	target, err := w.cfg.Meta.GetBucket(ctx, targetBucketName)
	if err != nil {
		return fmt.Errorf("get target bucket %q: %w", targetBucketName, err)
	}

	rows, err := w.collectRows(ctx, src, cfg)
	if err != nil {
		return fmt.Errorf("collect rows: %w", err)
	}

	csvBytes, err := encodeCSVGz(src.Name, rows)
	if err != nil {
		return fmt.Errorf("encode csv: %w", err)
	}
	csvSum := md5.Sum(csvBytes)
	csvETag := hex.EncodeToString(csvSum[:])

	now := w.cfg.Now().UTC()
	prefix := strings.TrimSuffix(cfg.Destination.S3BucketDestination.Prefix, "/")
	if prefix != "" {
		prefix += "/"
	}
	stamp := now.Format("20060102T150405Z")
	dataKey := fmt.Sprintf("%s%s/%s/%s/data.csv.gz", prefix, src.Name, configID, stamp)
	manifestKey := fmt.Sprintf("%s%s/%s/%s/manifest.json", prefix, src.Name, configID, stamp)

	if err := w.writeObject(ctx, target, dataKey, "application/octet-stream", csvBytes, csvETag); err != nil {
		return fmt.Errorf("write csv: %w", err)
	}

	manifestBytes, err := buildManifest(src.Name, cfg, dataKey, int64(len(csvBytes)), csvETag, now)
	if err != nil {
		return fmt.Errorf("build manifest: %w", err)
	}
	manifestSum := md5.Sum(manifestBytes)
	manifestETag := hex.EncodeToString(manifestSum[:])
	if err := w.writeObject(ctx, target, manifestKey, "application/json", manifestBytes, manifestETag); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}

	w.cfg.Logger.Info("inventory: report produced",
		"source_bucket", src.Name,
		"config_id", configID,
		"target_bucket", target.Name,
		"manifest_key", manifestKey,
		"data_key", dataKey,
		"rows", len(rows),
	)
	return nil
}

// row is one inventory CSV record collected from the source bucket.
type row struct {
	Bucket            string
	Key               string
	VersionID         string
	IsLatest          bool
	IsDeleteMarker    bool
	Size              int64
	LastModified      time.Time
	ETag              string
	StorageClass      string
	ChecksumAlgorithm string
}

func (w *Worker) collectRows(ctx context.Context, b *meta.Bucket, cfg *inventoryXML) ([]row, error) {
	prefix := ""
	if cfg.Filter != nil {
		prefix = cfg.Filter.Prefix
	}
	allVersions := strings.EqualFold(cfg.IncludedObjectVersions, "All")
	out := []row{}
	opts := meta.ListOptions{Prefix: prefix, Limit: 1000}
	if allVersions {
		for {
			res, err := w.cfg.Meta.ListObjectVersions(ctx, b.ID, opts)
			if err != nil {
				return nil, err
			}
			for _, v := range res.Versions {
				out = append(out, rowFromObject(b.Name, v))
			}
			if !res.Truncated {
				return out, nil
			}
			opts.Marker = res.NextKeyMarker
		}
	}
	for {
		res, err := w.cfg.Meta.ListObjects(ctx, b.ID, opts)
		if err != nil {
			return nil, err
		}
		for _, o := range res.Objects {
			r := rowFromObject(b.Name, o)
			r.IsLatest = true
			out = append(out, r)
		}
		if !res.Truncated {
			return out, nil
		}
		opts.Marker = res.NextMarker
	}
}

func rowFromObject(bucket string, o *meta.Object) row {
	algo := ""
	for k := range o.Checksums {
		// First non-empty algorithm wins; AWS only echoes one in CSV.
		if k != "" {
			algo = strings.ToUpper(k)
			break
		}
	}
	return row{
		Bucket:            bucket,
		Key:               o.Key,
		VersionID:         wireVersionID(o),
		IsLatest:          o.IsLatest,
		IsDeleteMarker:    o.IsDeleteMarker,
		Size:              o.Size,
		LastModified:      o.Mtime,
		ETag:              strings.Trim(o.ETag, "\""),
		StorageClass:      o.StorageClass,
		ChecksumAlgorithm: algo,
	}
}

// wireVersionID renders the version id for inventory CSV. Null-version rows
// (sentinel uuid) surface as "null", matching the wire form used by other
// surfaces like ListObjectVersions.
func wireVersionID(o *meta.Object) string {
	if o.IsNull || o.VersionID == meta.NullVersionID {
		return meta.NullVersionLiteral
	}
	return o.VersionID
}

func encodeCSVGz(bucketName string, rows []row) ([]byte, error) {
	var raw bytes.Buffer
	cw := csv.NewWriter(&raw)
	if err := cw.Write(CSVHeader); err != nil {
		return nil, err
	}
	for _, r := range rows {
		_ = bucketName
		rec := []string{
			r.Bucket,
			r.Key,
			r.VersionID,
			strconv.FormatBool(r.IsLatest),
			strconv.FormatBool(r.IsDeleteMarker),
			strconv.FormatInt(r.Size, 10),
			r.LastModified.UTC().Format(time.RFC3339),
			r.ETag,
			r.StorageClass,
			r.ChecksumAlgorithm,
		}
		if err := cw.Write(rec); err != nil {
			return nil, err
		}
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return nil, err
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(raw.Bytes()); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return gz.Bytes(), nil
}

// manifestFile is one entry in the manifest.files array.
type manifestFile struct {
	Key         string `json:"key"`
	Size        int64  `json:"size"`
	MD5checksum string `json:"MD5checksum"`
}

type manifestJSON struct {
	SourceBucket           string         `json:"sourceBucket"`
	DestinationBucket      string         `json:"destinationBucket"`
	Version                string         `json:"version"`
	CreationTimestamp      string         `json:"creationTimestamp"`
	FileFormat             string         `json:"fileFormat"`
	FileSchema             string         `json:"fileSchema"`
	Files                  []manifestFile `json:"files"`
	IncludedObjectVersions string         `json:"includedObjectVersions,omitempty"`
}

func buildManifest(sourceBucket string, cfg *inventoryXML, dataKey string, dataSize int64, dataMD5 string, now time.Time) ([]byte, error) {
	dest := cfg.Destination.S3BucketDestination
	m := manifestJSON{
		SourceBucket:      sourceBucket,
		DestinationBucket: dest.Bucket,
		Version:           "2016-11-30",
		CreationTimestamp: strconv.FormatInt(now.UnixMilli(), 10),
		FileFormat:        "CSV",
		FileSchema:        strings.Join(CSVHeader, ", "),
		Files: []manifestFile{
			{Key: dataKey, Size: dataSize, MD5checksum: dataMD5},
		},
		IncludedObjectVersions: cfg.IncludedObjectVersions,
	}
	return json.Marshal(m)
}

func (w *Worker) writeObject(ctx context.Context, target *meta.Bucket, key, contentType string, body []byte, etag string) error {
	manifest, err := w.cfg.Data.PutChunks(ctx, bytes.NewReader(body), target.DefaultClass)
	if err != nil {
		return err
	}
	manifest.Size = int64(len(body))
	manifest.ETag = etag
	versioned := meta.IsVersioningActive(target.Versioning)
	o := &meta.Object{
		BucketID:     target.ID,
		Key:          key,
		Size:         int64(len(body)),
		ETag:         etag,
		ContentType:  contentType,
		StorageClass: target.DefaultClass,
		Mtime:        w.cfg.Now().UTC(),
		Manifest:     manifest,
		IsLatest:     true,
	}
	if err := w.cfg.Meta.PutObject(ctx, o, versioned); err != nil {
		_ = w.cfg.Data.Delete(ctx, manifest)
		return err
	}
	return nil
}

// stripBucketARN strips the AWS S3 ARN prefix so the destination resolves to a
// bare bucket name on the local Strata cluster.
func stripBucketARN(s string) string {
	if rest, ok := strings.CutPrefix(s, "arn:aws:s3:::"); ok {
		return rest
	}
	return s
}

// DecodeCSVGz is a test-friendly inverse of encodeCSVGz used by both the worker
// suite and any external pipeline that wants to assert on emitted reports.
func DecodeCSVGz(blob []byte) ([][]string, error) {
	zr, err := gzip.NewReader(bytes.NewReader(blob))
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		return nil, err
	}
	cr := csv.NewReader(bytes.NewReader(raw))
	return cr.ReadAll()
}
