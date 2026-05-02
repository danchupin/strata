// Package auditexport drains aged audit_log partitions, writes one
// gzipped JSON-lines object per (bucket, day) partition into the
// configured export bucket, and deletes the source partition once the
// export upload succeeds. Drives cmd/strata-audit-export (US-046).
package auditexport

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/meta"
)

// Config wires the audit-export Worker. New() applies defaults: After=30d,
// Interval=24h, Logger=slog.Default, Now=time.Now.
type Config struct {
	Meta     meta.Store
	Data     data.Backend
	Bucket   string
	Prefix   string
	After    time.Duration
	Interval time.Duration
	Logger   *slog.Logger
	Now      func() time.Time
}

// Worker exports aged audit_log partitions on a daily tick.
type Worker struct {
	cfg Config
}

func New(cfg Config) (*Worker, error) {
	if cfg.Meta == nil {
		return nil, errors.New("auditexport: meta store required")
	}
	if cfg.Data == nil {
		return nil, errors.New("auditexport: data backend required")
	}
	if strings.TrimSpace(cfg.Bucket) == "" {
		return nil, errors.New("auditexport: target bucket required")
	}
	if cfg.After <= 0 {
		cfg.After = 30 * 24 * time.Hour
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 24 * time.Hour
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Worker{cfg: cfg}, nil
}

// Run loops on cfg.Interval until ctx is cancelled. RunOnce is invoked at
// startup so the first export does not wait a full Interval.
func (w *Worker) Run(ctx context.Context) error {
	w.cfg.Logger.Info("auditexport: starting",
		"interval", w.cfg.Interval, "after", w.cfg.After,
		"bucket", w.cfg.Bucket, "prefix", w.cfg.Prefix)
	if err := w.RunOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
		w.cfg.Logger.Warn("auditexport: initial tick failed", "error", err.Error())
	}
	ticker := time.NewTicker(w.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := w.RunOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
				w.cfg.Logger.Warn("auditexport: tick failed", "error", err.Error())
			}
		}
	}
}

// RunOnce performs a single drain pass: enumerates aged partitions, writes
// one gzipped JSON-lines object per partition into the configured bucket,
// then deletes the source partition. Returns nil if the target bucket is
// missing — the worker logs and skips so a misconfigured operator can
// repair without restart.
func (w *Worker) RunOnce(ctx context.Context) error {
	tgt, err := w.cfg.Meta.GetBucket(ctx, w.cfg.Bucket)
	if err != nil {
		if errors.Is(err, meta.ErrBucketNotFound) {
			w.cfg.Logger.Warn("auditexport: target bucket missing", "bucket", w.cfg.Bucket)
			return nil
		}
		return fmt.Errorf("get target bucket %q: %w", w.cfg.Bucket, err)
	}
	cutoff := w.cfg.Now().Add(-w.cfg.After)
	parts, err := w.cfg.Meta.ListAuditPartitionsBefore(ctx, cutoff)
	if err != nil {
		return fmt.Errorf("list partitions: %w", err)
	}
	for _, p := range parts {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := w.exportPartition(ctx, tgt, p); err != nil {
			w.cfg.Logger.Warn("auditexport: partition export failed",
				"bucket_id", p.BucketID, "day", p.Day.Format("2006-01-02"),
				"error", err.Error())
		}
	}
	return nil
}

func (w *Worker) exportPartition(ctx context.Context, tgt *meta.Bucket, p meta.AuditPartition) error {
	rows, err := w.cfg.Meta.ReadAuditPartition(ctx, p.BucketID, p.Day)
	if err != nil {
		return fmt.Errorf("read partition: %w", err)
	}
	if len(rows) == 0 {
		// Nothing to upload but the partition entry survived a crash mid-delete;
		// run the delete again so the next tick sees a clean state.
		if err := w.cfg.Meta.DeleteAuditPartition(ctx, p.BucketID, p.Day); err != nil {
			return fmt.Errorf("delete empty partition: %w", err)
		}
		return nil
	}
	body, err := encodeJSONLinesGzip(rows)
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}
	key := exportKey(w.cfg.Prefix, p)
	mf, err := w.cfg.Data.PutChunks(ctx, bytes.NewReader(body), tgt.DefaultClass)
	if err != nil {
		return fmt.Errorf("put chunks: %w", err)
	}
	obj := &meta.Object{
		BucketID:     tgt.ID,
		Key:          key,
		Size:         mf.Size,
		ETag:         mf.ETag,
		ContentType:  "application/x-ndjson+gzip",
		StorageClass: mf.Class,
		Mtime:        w.cfg.Now().UTC(),
		Manifest:     mf,
	}
	if err := w.cfg.Meta.PutObject(ctx, obj, meta.IsVersioningActive(tgt.Versioning)); err != nil {
		_ = w.cfg.Data.Delete(ctx, mf)
		return fmt.Errorf("put object: %w", err)
	}
	if err := w.cfg.Meta.DeleteAuditPartition(ctx, p.BucketID, p.Day); err != nil {
		return fmt.Errorf("delete source partition: %w", err)
	}
	w.cfg.Logger.Info("auditexport: exported partition",
		"bucket_id", p.BucketID, "day", p.Day.Format("2006-01-02"),
		"rows", len(rows), "bytes", len(body),
		"target_bucket", tgt.Name, "target_key", key)
	return nil
}

// exportKey produces the destination object key for one partition. Keys
// follow `<prefix>YYYY-MM-DD/<bucket-name>-<bucket-id>.jsonl.gz` so a
// listing in the export bucket sorts by day then by source bucket. The
// bucket-id suffix avoids collisions if the same human-readable name is
// reused across deletes.
func exportKey(prefix string, p meta.AuditPartition) string {
	day := p.Day.UTC().Format("2006-01-02")
	bucketName := p.Bucket
	if bucketName == "" {
		bucketName = "-"
	}
	return prefix + day + "/" + bucketName + "-" + idSuffix(p.BucketID) + ".jsonl.gz"
}

func idSuffix(id uuid.UUID) string {
	if id == uuid.Nil {
		return "iam"
	}
	return id.String()
}

func encodeJSONLinesGzip(rows []meta.AuditEvent) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	enc := json.NewEncoder(gz)
	for i := range rows {
		if err := enc.Encode(rows[i]); err != nil {
			return nil, err
		}
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// DecodeJSONLinesGzip is the inverse of encodeJSONLinesGzip; exported so
// tests (and any future verifier) can round-trip the export format
// without re-implementing the framing.
func DecodeJSONLinesGzip(body []byte) ([]meta.AuditEvent, error) {
	gz, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	dec := json.NewDecoder(gz)
	var out []meta.AuditEvent
	for {
		var e meta.AuditEvent
		if err := dec.Decode(&e); err != nil {
			if errors.Is(err, io.EOF) {
				return out, nil
			}
			return nil, err
		}
		out = append(out, e)
	}
}
