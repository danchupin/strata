// Package accesslog drains the access_log_buffer table populated by the
// gateway's AccessLogMiddleware (US-013) and writes one AWS-format
// server-access-log file per source bucket per flush into the target bucket
// configured by PutBucketLogging.
package accesslog

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/meta"
	strataotel "github.com/danchupin/strata/internal/otel"
)

// Config wires the access-log Worker. Defaults applied in New: Interval=5min,
// MaxFlushBytes=5 MiB, PollLimit=10000, Logger=slog.Default, Now=time.Now.
type Config struct {
	Meta          meta.Store
	Data          data.Backend
	Logger        *slog.Logger
	Interval      time.Duration
	MaxFlushBytes int64
	PollLimit     int
	Now           func() time.Time
	// Tracer emits per-iteration parent spans (`worker.access-log.tick`) plus
	// `access_log.flush_bucket` sub-op children. Nil falls back to a process-
	// shared no-op tracer.
	Tracer trace.Tracer
}

// Worker drains buffered access-log rows per source-bucket, formats them into
// AWS-compatible log lines, writes one object per flush into the source
// bucket's configured target bucket, then acks the drained rows.
type Worker struct {
	cfg Config

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

func New(cfg Config) (*Worker, error) {
	if cfg.Meta == nil {
		return nil, errors.New("accesslog: meta store required")
	}
	if cfg.Data == nil {
		return nil, errors.New("accesslog: data backend required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 5 * time.Minute
	}
	if cfg.MaxFlushBytes <= 0 {
		cfg.MaxFlushBytes = 5 * 1024 * 1024
	}
	if cfg.PollLimit <= 0 {
		cfg.PollLimit = 10000
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Worker{cfg: cfg}, nil
}

// Run loops on cfg.Interval, flushing every source bucket until ctx is
// cancelled.
func (w *Worker) Run(ctx context.Context) error {
	w.cfg.Logger.Info("accesslog: starting", "interval", w.cfg.Interval, "max_flush_bytes", w.cfg.MaxFlushBytes)
	ticker := time.NewTicker(w.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := w.RunOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
				w.cfg.Logger.Warn("accesslog: tick failed", "error", err.Error())
			}
		}
	}
}

// RunOnce performs a single drain-and-flush pass over every bucket. Exposed
// for tests + the cmd binary's --once flag.
func (w *Worker) RunOnce(ctx context.Context) error {
	iterCtx, span := strataotel.StartIteration(ctx, w.tracerOrNoop(), "access-log")
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
		bucketCtx, bucketSpan := w.tracerOrNoop().Start(ctx, "access_log.flush_bucket",
			trace.WithSpanKind(trace.SpanKindInternal),
			trace.WithAttributes(
				strataotel.AttrComponentWorker,
				attribute.String(strataotel.WorkerKey, "access-log"),
				attribute.String("strata.access_log.bucket", b.Name),
				attribute.String("strata.access_log.bucket_id", b.ID.String()),
			),
		)
		if err := w.flushBucket(bucketCtx, b); err != nil {
			bucketSpan.RecordError(err)
			bucketSpan.SetStatus(codes.Error, err.Error())
			w.recordIterErr(err)
			w.cfg.Logger.Warn("accesslog: flush bucket failed", "bucket", b.Name, "error", err.Error())
		}
		bucketSpan.End()
	}
	return nil
}

func (w *Worker) flushBucket(ctx context.Context, src *meta.Bucket) error {
	blob, err := w.cfg.Meta.GetBucketLogging(ctx, src.ID)
	if err != nil {
		if errors.Is(err, meta.ErrNoSuchLogging) {
			return nil
		}
		return fmt.Errorf("get logging: %w", err)
	}
	target, prefix, err := parseLoggingTarget(blob)
	if err != nil {
		return fmt.Errorf("parse logging target: %w", err)
	}
	if target == "" {
		return nil
	}
	rows, err := w.cfg.Meta.ListPendingAccessLog(ctx, src.ID, w.cfg.PollLimit)
	if err != nil {
		return fmt.Errorf("list pending: %w", err)
	}
	if len(rows) == 0 {
		return nil
	}
	tgtBucket, err := w.cfg.Meta.GetBucket(ctx, target)
	if err != nil {
		return fmt.Errorf("get target bucket %q: %w", target, err)
	}
	return w.flushRows(ctx, src, tgtBucket, prefix, rows)
}

// flushRows splits the drained rows into chunks bounded by MaxFlushBytes,
// writes one object per chunk, and acks each successfully flushed row.
func (w *Worker) flushRows(ctx context.Context, src, tgt *meta.Bucket, prefix string, rows []meta.AccessLogEntry) error {
	var (
		buf       bytes.Buffer
		batch     []meta.AccessLogEntry
		flushedAt = w.cfg.Now()
	)
	flush := func() error {
		if buf.Len() == 0 {
			return nil
		}
		if err := w.writeFlush(ctx, src, tgt, prefix, flushedAt, len(batch), buf.Bytes()); err != nil {
			return err
		}
		for _, r := range batch {
			if err := w.cfg.Meta.AckAccessLog(ctx, r); err != nil {
				w.cfg.Logger.Warn("accesslog: ack failed", "event_id", r.EventID, "error", err.Error())
			}
		}
		buf.Reset()
		batch = batch[:0]
		flushedAt = w.cfg.Now()
		return nil
	}
	owner := src.Owner
	for _, row := range rows {
		line := FormatLine(owner, row) + "\n"
		if buf.Len() > 0 && int64(buf.Len()+len(line)) > w.cfg.MaxFlushBytes {
			if err := flush(); err != nil {
				return err
			}
		}
		buf.WriteString(line)
		batch = append(batch, row)
	}
	return flush()
}

func (w *Worker) writeFlush(ctx context.Context, src, tgt *meta.Bucket, prefix string, now time.Time, rowCount int, body []byte) error {
	key := flushFileName(prefix, src.Name, now, rowCount)
	mf, err := w.cfg.Data.PutChunks(ctx, bytes.NewReader(body), tgt.DefaultClass)
	if err != nil {
		return fmt.Errorf("put chunks: %w", err)
	}
	obj := &meta.Object{
		BucketID:     tgt.ID,
		Key:          key,
		Size:         mf.Size,
		ETag:         mf.ETag,
		ContentType:  "text/plain",
		StorageClass: mf.Class,
		Mtime:        now.UTC(),
		Manifest:     mf,
	}
	if err := w.cfg.Meta.PutObject(ctx, obj, meta.IsVersioningActive(tgt.Versioning)); err != nil {
		_ = w.cfg.Data.Delete(ctx, mf)
		return fmt.Errorf("put object: %w", err)
	}
	w.cfg.Logger.Info("accesslog: flushed",
		"source", src.Name, "target", tgt.Name, "key", key, "rows", rowCount, "bytes", len(body))
	return nil
}

// loggingDoc mirrors the AWS BucketLoggingStatus envelope; we only need the
// destination fields. The full XML round-tripping lives in internal/s3api.
type loggingDoc struct {
	XMLName        xml.Name `xml:"BucketLoggingStatus"`
	LoggingEnabled *struct {
		TargetBucket string `xml:"TargetBucket"`
		TargetPrefix string `xml:"TargetPrefix"`
	} `xml:"LoggingEnabled,omitempty"`
}

func parseLoggingTarget(blob []byte) (target, prefix string, err error) {
	var doc loggingDoc
	if err := xml.Unmarshal(blob, &doc); err != nil {
		return "", "", err
	}
	if doc.LoggingEnabled == nil {
		return "", "", nil
	}
	return strings.TrimSpace(doc.LoggingEnabled.TargetBucket), doc.LoggingEnabled.TargetPrefix, nil
}

