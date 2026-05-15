package cassandra

import (
	"context"
	"log/slog"
	"time"

	"github.com/gocql/gocql"
)

// LookupReconcileReport summarises one pass of ReconcileLookupTables. The
// counts reflect rows actually written; idempotent re-runs report zero
// `*Written` after the first successful boot.
type LookupReconcileReport struct {
	GCScanned         int
	GCWritten         int
	MultipartScanned  int
	MultipartWritten  int
	Duration          time.Duration
}

// ReconcileLookupTables walks the legacy `gc_entries_v2` and
// `multipart_uploads` partitions once, mirroring every row that carries a
// non-empty cluster id into the denormalised `_by_cluster` lookup tables
// (US-005). Cassandra `INSERT` is upsert with no LWT — idempotent re-runs
// overwrite existing lookup rows with the same payload at no semantic cost.
//
// The reconcile is intentionally synchronous and bounded by the size of the
// source tables — operators upgrading deployments with multi-million-row
// GC backlogs should expect a longer boot pause; the per-1000-row progress
// log line gives operational visibility.
func (s *Store) ReconcileLookupTables(ctx context.Context, logger *slog.Logger) (LookupReconcileReport, error) {
	start := time.Now()
	rep := LookupReconcileReport{}

	if err := s.reconcileGCLookup(ctx, logger, &rep); err != nil {
		return rep, err
	}
	if err := s.reconcileMultipartLookup(ctx, logger, &rep); err != nil {
		return rep, err
	}

	rep.Duration = time.Since(start)
	if logger != nil {
		logger.Info("cluster reconcile lookup tables",
			"gc_entries", rep.GCScanned,
			"gc_written", rep.GCWritten,
			"multipart_uploads", rep.MultipartScanned,
			"multipart_written", rep.MultipartWritten,
			"duration_ms", rep.Duration.Milliseconds(),
		)
	}
	return rep, nil
}

func (s *Store) reconcileGCLookup(ctx context.Context, logger *slog.Logger, rep *LookupReconcileReport) error {
	iter := s.s.Query(
		`SELECT region, enqueued_at, oid, pool, cluster, namespace FROM gc_entries_v2`,
	).WithContext(ctx).PageSize(1000).Iter()
	var (
		region, oid, pool, cluster, namespace string
		enqueuedAt                            time.Time
	)
	for iter.Scan(&region, &enqueuedAt, &oid, &pool, &cluster, &namespace) {
		rep.GCScanned++
		if cluster == "" {
			continue
		}
		if err := s.s.Query(
			`INSERT INTO gc_entries_by_cluster (cluster, region, enqueued_at, oid, pool, namespace)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			cluster, region, enqueuedAt, oid, pool, namespace,
		).WithContext(ctx).Exec(); err != nil {
			if logger != nil {
				logger.Warn("reconcile gc lookup row",
					"cluster", cluster, "oid", oid, "error", err.Error())
			}
			continue
		}
		rep.GCWritten++
		if logger != nil && rep.GCWritten%1000 == 0 {
			logger.Info("reconcile gc lookup progress",
				"written", rep.GCWritten, "scanned", rep.GCScanned)
		}
	}
	return iter.Close()
}

func (s *Store) reconcileMultipartLookup(ctx context.Context, logger *slog.Logger, rep *LookupReconcileReport) error {
	iter := s.s.Query(
		`SELECT bucket_id, upload_id, key, cluster FROM multipart_uploads`,
	).WithContext(ctx).PageSize(1000).Iter()
	var (
		bucketID, uploadID gocql.UUID
		key, cluster       string
	)
	for iter.Scan(&bucketID, &uploadID, &key, &cluster) {
		rep.MultipartScanned++
		if cluster == "" {
			continue
		}
		if err := s.s.Query(
			`INSERT INTO multipart_uploads_by_cluster (cluster, bucket_id, upload_id, key)
			 VALUES (?, ?, ?, ?)`,
			cluster, bucketID, uploadID, key,
		).WithContext(ctx).Exec(); err != nil {
			if logger != nil {
				logger.Warn("reconcile multipart lookup row",
					"cluster", cluster, "upload_id", uploadID.String(), "error", err.Error())
			}
			continue
		}
		rep.MultipartWritten++
		if logger != nil && rep.MultipartWritten%1000 == 0 {
			logger.Info("reconcile multipart lookup progress",
				"written", rep.MultipartWritten, "scanned", rep.MultipartScanned)
		}
	}
	return iter.Close()
}
