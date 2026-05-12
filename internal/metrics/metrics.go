package metrics

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	HTTPRequests = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "strata_http_requests_total",
			Help: "Total HTTP requests served by the gateway, partitioned by method, response code, bucket, and access_key. bucket=\"_admin\" for /admin/v1, /metrics, /healthz, /readyz, /console; bucket=\"_root\" for the empty path. access_key=\"_anon\" for unauthenticated requests.",
		},
		[]string{"method", "code", "bucket", "access_key"},
	)

	HTTPDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "strata_http_request_duration_seconds",
			Help:    "Latency of HTTP requests served by the gateway, partitioned by method, templated path, and response status.",
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
		},
		[]string{"method", "path", "status"},
	)

	CassandraQueryDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "strata_cassandra_query_duration_seconds",
			Help:    "Latency of Cassandra queries observed via the gocql QueryObserver hook, partitioned by table and op.",
			Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
		},
		[]string{"table", "op"},
	)

	GCEnqueued = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "strata_gc_enqueued_chunks_total",
		Help: "RADOS chunks enqueued for async deletion.",
	})

	GCProcessed = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "strata_gc_processed_chunks_total",
		Help: "RADOS chunks successfully deleted by the GC worker.",
	})

	LifecycleTransitions = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "strata_lifecycle_transitions_total",
			Help: "Objects moved between storage classes by the lifecycle worker.",
		},
		[]string{"target_class"},
	)

	LifecycleExpirations = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "strata_lifecycle_expirations_total",
		Help: "Objects removed by the lifecycle worker.",
	})

	ReplicationLagSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "strata_replication_lag_seconds",
			Help:    "Time between source-write event_time and replication-worker terminal outcome (success or FAILED).",
			Buckets: []float64{0.1, 0.5, 1, 2.5, 5, 10, 30, 60, 300, 900, 3600},
		},
		[]string{"rule_id"},
	)

	ReplicationCompleted = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "strata_replication_completed_total",
			Help: "Replication events successfully delivered to the peer.",
		},
		[]string{"rule_id"},
	)

	ReplicationFailed = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "strata_replication_failed_total",
			Help: "Replication events that exhausted their retry budget and were marked FAILED.",
		},
		[]string{"rule_id"},
	)

	ReplicationQueueDepth = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "strata_replication_queue_depth",
			Help: "Pending replication_queue rows per replication rule, sampled by the replicator worker.",
		},
		[]string{"rule_id"},
	)

	ReplicationQueueAge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "strata_replication_queue_age_seconds",
			Help: "Oldest pending replication_queue row age (seconds) per source bucket, sampled by the replicator worker. Backs the per-bucket Replication tab (US-014).",
		},
		[]string{"bucket"},
	)

	RADOSOpDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "strata_rados_op_duration_seconds",
			Help:    "Latency of RADOS operations (put/get/del) per pool.",
			Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
		},
		[]string{"pool", "op"},
	)

	GCQueueDepth = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "strata_gc_queue_depth",
			Help: "Pending gc_queue rows per region, sampled by the GC worker.",
		},
		[]string{"region"},
	)

	MultipartActive = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "strata_multipart_active",
			Help: "Active multipart uploads per bucket; incremented on InitiateMultipart, decremented on Complete or Abort.",
		},
		[]string{"bucket"},
	)

	BucketBytes = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "strata_bucket_bytes",
			Help: "Total object bytes per bucket and storage class, sampled hourly by the gateway.",
		},
		[]string{"bucket", "storage_class"},
	)

	BucketShardBytes = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "strata_bucket_shard_bytes",
			Help: "Total object bytes per (bucket, shard) for the top-N largest buckets (US-012). Backs the Distribution tab (US-013). Cardinality bound: top-N buckets * shard_count.",
		},
		[]string{"bucket", "shard"},
	)

	BucketShardObjects = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "strata_bucket_shard_objects",
			Help: "Total object count per (bucket, shard) for the top-N largest buckets (US-012). Backs the Distribution tab (US-013). Cardinality bound: top-N buckets * shard_count.",
		},
		[]string{"bucket", "shard"},
	)

	StorageClassBytes = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "strata_storage_class_bytes",
			Help: "Total object bytes per (storage_class, bucket) for the top-N largest buckets (US-003 storage cycle). Cardinality bound: top-N buckets * |classes|.",
		},
		[]string{"class", "bucket"},
	)

	StorageClassObjects = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "strata_storage_class_objects",
			Help: "Total object count per (storage_class, bucket) for the top-N largest buckets (US-003 storage cycle). Cardinality bound: top-N buckets * |classes|.",
		},
		[]string{"class", "bucket"},
	)

	LifecycleTickTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "strata_lifecycle_tick_total",
			Help: "Lifecycle worker per-action outcomes; action=transition|expire|expire_noncurrent|abort_multipart, status=success|error|skipped.",
		},
		[]string{"action", "status"},
	)

	WorkerPanicTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "strata_worker_panic_total",
			Help: "Number of panics caught and recovered by the worker supervisor, per worker name. shard='-' for non-sharded workers; for the gc fan-out (US-004) shard carries the per-shard index 0..STRATA_GC_SHARDS-1.",
		},
		[]string{"worker", "shard"},
	)

	NotifyDeliveryTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "strata_notify_delivery_total",
			Help: "Notify worker delivery outcomes per sink; status=success|failure|dlq.",
		},
		[]string{"sink", "status"},
	)

	MetaTikvAuditSweepDeleted = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "strata_meta_tikv_audit_sweep_deleted_total",
		Help: "Audit rows expunged by the TiKV audit-retention sweeper (TiKV has no native TTL).",
	})

	AuditStreamSubscribers = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "strata_audit_stream_subscribers",
		Help: "Live audit-tail subscribers attached to the in-process auditstream.Broadcaster.",
	})

	OTelRingbufTraces = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "strata_otel_ringbuf_traces",
		Help: "Traces retained in the in-process OTel ring buffer (US-005).",
	})

	OTelRingbufEvicted = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "strata_otel_ringbuf_evicted_total",
		Help: "Traces evicted from the in-process OTel ring buffer due to bytes-budget pressure (US-005).",
	})

	CassandraLWTConflictsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "strata_cassandra_lwt_conflicts_total",
			Help: "Cassandra LWT (compare-and-set) conflicts per (table, bucket, shard); incremented when applied=false. Backs the Hot Shards heatmap (US-009). Cardinality bound: ~1000 buckets * 64 shards.",
		},
		[]string{"table", "bucket", "shard"},
	)

	QuotaReconcileDriftBytes = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "strata_quota_reconcile_drift_bytes",
			Help: "Last observed drift between bucket_stats.used_bytes and the actual sum of live (non-delete-marker) object sizes per bucket, sampled by the quota-reconcile worker (US-007). Positive = stats undercount (real data is larger); negative = stats overcount.",
		},
		[]string{"bucket"},
	)

	RebalancePlannedMovesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "strata_rebalance_planned_moves_total",
			Help: "Chunk moves planned by the rebalance worker per bucket (US-003). One increment per chunk whose current cluster does not match placement.PickCluster's verdict at scan time. Mover side counters land in US-004/US-005.",
		},
		[]string{"bucket"},
	)

	RebalanceBytesMovedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "strata_rebalance_bytes_moved_total",
			Help: "Bytes copied between data clusters by the rebalance worker (US-004 RADOS / US-005 S3). Counted on the target write so retried reads do not double-count.",
		},
		[]string{"from", "to"},
	)

	RebalanceChunksMovedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "strata_rebalance_chunks_moved_total",
			Help: "Chunks successfully copied between clusters by the rebalance worker (US-004/US-005). Incremented once per chunk after the target write returns.",
		},
		[]string{"from", "to", "bucket"},
	)

	RebalanceCASConflictsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "strata_rebalance_cas_conflicts_total",
			Help: "Manifest SetObjectStorage CAS conflicts during a rebalance move (US-004/US-005). Incremented when a concurrent client write wins the LWT and the freshly-copied target chunks get enqueued into the GC queue.",
		},
		[]string{"bucket"},
	)

	RebalanceRefusedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "strata_rebalance_refused_total",
			Help: "Rebalance moves refused by the worker's safety rails (US-006). Reason is one of target_full (target cluster.used/total > 0.90 RADOS-only) or target_draining (target cluster is in meta.ClusterStateDraining). Per-target visibility lets operators spot a stuck drain.",
		},
		[]string{"reason", "target"},
	)
)

func Register() {
	prometheus.MustRegister(
		HTTPRequests, HTTPDuration,
		CassandraQueryDuration,
		GCEnqueued, GCProcessed,
		LifecycleTransitions, LifecycleExpirations,
		ReplicationLagSeconds, ReplicationCompleted, ReplicationFailed,
		ReplicationQueueDepth, ReplicationQueueAge,
		RADOSOpDuration,
		GCQueueDepth,
		MultipartActive,
		BucketBytes,
		BucketShardBytes,
		BucketShardObjects,
		StorageClassBytes,
		StorageClassObjects,
		LifecycleTickTotal,
		NotifyDeliveryTotal,
		WorkerPanicTotal,
		MetaTikvAuditSweepDeleted,
		AuditStreamSubscribers,
		OTelRingbufTraces,
		OTelRingbufEvicted,
		CassandraLWTConflictsTotal,
		QuotaReconcileDriftBytes,
		RebalancePlannedMovesTotal,
		RebalanceBytesMovedTotal,
		RebalanceChunksMovedTotal,
		RebalanceCASConflictsTotal,
		RebalanceRefusedTotal,
	)
}

func Handler() http.Handler { return promhttp.Handler() }

type wrappedWriter struct {
	http.ResponseWriter
	status int
}

func (w *wrappedWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// HTTPMetricsLabeler resolves per-request access_key for the
// strata_http_requests_total counter. ObserveHTTP runs in `internal/metrics`
// which must not import auth (it would create a cycle with `internal/auth`),
// so the gateway wires this hook in `internal/serverapp` to call
// `auth.FromContext`.
//
// nil hook → access_key="_anon" (default during early-boot wiring; the
// gateway sets the hook before serving traffic).
var HTTPMetricsLabeler func(*http.Request) (accessKey string)

func ObserveHTTP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &wrappedWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r)
		code := strconv.Itoa(rw.status)
		bucket := bucketLabel(r.URL.Path)
		accessKey := "_anon"
		if HTTPMetricsLabeler != nil {
			if k := HTTPMetricsLabeler(r); k != "" {
				accessKey = k
			}
		}
		HTTPRequests.WithLabelValues(r.Method, code, bucket, accessKey).Inc()
		HTTPDuration.WithLabelValues(r.Method, TemplatePath(r.URL.Path), code).Observe(time.Since(start).Seconds())
	})
}

// bucketLabel extracts the bucket portion of the URL path for the
// strata_http_requests_total bucket label. Path-style S3 URLs put the
// bucket as the first segment (e.g. `/lab-test/file.txt` → `lab-test`).
// Admin / observability endpoints share the literal "_admin" value to keep
// cardinality bounded.
func bucketLabel(p string) string {
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		return "_root"
	}
	first := p
	if i := strings.IndexByte(p, '/'); i >= 0 {
		first = p[:i]
	}
	switch first {
	case "admin", "metrics", "healthz", "readyz", "console":
		return "_admin"
	}
	return first
}

// TemplatePath collapses a URL path into a low-cardinality label for the
// http_request_duration_seconds histogram. Bucket and key segments become
// {bucket} / {key} placeholders; admin endpoints (/metrics, /healthz, /readyz)
// keep their literal path. Anything else falls back to the bucket/key shape.
func TemplatePath(p string) string {
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		return "/"
	}
	switch p {
	case "metrics", "healthz", "readyz":
		return "/" + p
	}
	if strings.Contains(p, "/") {
		return "/{bucket}/{key}"
	}
	return "/{bucket}"
}

// CassandraObserver implements the cassandra.Metrics interface defined in
// internal/meta/cassandra. The cassandra package keeps prometheus out of its
// import set; the binary wiring layer plugs in this adapter.
type CassandraObserver struct{}

func (CassandraObserver) ObserveQuery(table, op string, duration time.Duration, err error) {
	if table == "" {
		table = "unknown"
	}
	if op == "" {
		op = "UNKNOWN"
	}
	CassandraQueryDuration.WithLabelValues(table, op).Observe(duration.Seconds())
}

// IncLWTConflict bumps the Hot Shards LWT-conflict counter (US-009). Empty
// labels collapse to "unknown" / "-" placeholders so a missing bucket-name
// resolution never silently drops the conflict.
func (CassandraObserver) IncLWTConflict(table, bucket, shard string) {
	if table == "" {
		table = "unknown"
	}
	if bucket == "" {
		bucket = "-"
	}
	if shard == "" {
		shard = "-"
	}
	CassandraLWTConflictsTotal.WithLabelValues(table, bucket, shard).Inc()
}

// RADOSObserver implements the rados.Metrics interface. Cmd-layer adapter so
// internal/data/rados stays free of prometheus imports.
type RADOSObserver struct{}

func (RADOSObserver) ObserveOp(pool, op string, duration time.Duration, err error) {
	if pool == "" {
		pool = "unknown"
	}
	if op == "" {
		op = "unknown"
	}
	RADOSOpDuration.WithLabelValues(pool, op).Observe(duration.Seconds())
}

// GCObserver implements the gc.Metrics interface. SetQueueDepth updates the
// per-region gauge sampled at each drain tick.
type GCObserver struct{}

func (GCObserver) SetQueueDepth(region string, depth int) {
	if region == "" {
		region = "default"
	}
	GCQueueDepth.WithLabelValues(region).Set(float64(depth))
}

// NotifyObserver implements the notify.Metrics interface. status ∈
// {success, failure, dlq}.
type NotifyObserver struct{}

func (NotifyObserver) IncDelivery(sink, status string) {
	if sink == "" {
		sink = "unknown"
	}
	if status == "" {
		status = "unknown"
	}
	NotifyDeliveryTotal.WithLabelValues(sink, status).Inc()
}

// LifecycleObserver implements the lifecycle.Metrics interface. action ∈
// {transition, expire, expire_noncurrent, abort_multipart}; status ∈
// {success, error, skipped}.
type LifecycleObserver struct{}

func (LifecycleObserver) IncTick(action, status string) {
	if action == "" {
		action = "unknown"
	}
	if status == "" {
		status = "unknown"
	}
	LifecycleTickTotal.WithLabelValues(action, status).Inc()
}

// BucketStatsObserver implements the bucketstats.Sink interface. The
// hourly sampler updates BucketBytes per (bucket, storage_class).
type BucketStatsObserver struct{}

func (BucketStatsObserver) SetBucketBytes(bucket, class string, bytes int64) {
	if bucket == "" {
		bucket = "unknown"
	}
	if class == "" {
		class = "STANDARD"
	}
	BucketBytes.WithLabelValues(bucket, class).Set(float64(bytes))
}

// SetBucketShardBytes / SetBucketShardObjects publish per-shard distribution
// gauges populated by the bucketstats sampler for the top-N buckets (US-012).
// shard is the integer partition index; the label is stringified once at the
// adapter so prometheus stays string-typed.
func (BucketStatsObserver) SetBucketShardBytes(bucket string, shard int, bytes int64) {
	if bucket == "" {
		bucket = "unknown"
	}
	BucketShardBytes.WithLabelValues(bucket, strconv.Itoa(shard)).Set(float64(bytes))
}

func (BucketStatsObserver) SetBucketShardObjects(bucket string, shard int, objects int64) {
	if bucket == "" {
		bucket = "unknown"
	}
	BucketShardObjects.WithLabelValues(bucket, strconv.Itoa(shard)).Set(float64(objects))
}

// ResetBucketShard removes per-(bucket, shard) gauge series so a freshly
// dropped-from-top-N bucket does not linger as stale data in the
// strata_bucket_shard_* metrics. The sampler invokes this between passes for
// any bucket that exited the top-N window.
func (BucketStatsObserver) ResetBucketShard(bucket string) {
	if bucket == "" {
		return
	}
	BucketShardBytes.DeletePartialMatch(prometheus.Labels{"bucket": bucket})
	BucketShardObjects.DeletePartialMatch(prometheus.Labels{"bucket": bucket})
}

// SetStorageClassBytes / SetStorageClassObjects publish per-(bucket, class)
// gauges populated by the bucketstats sampler for the top-N buckets (US-003
// storage cycle).
func (BucketStatsObserver) SetStorageClassBytes(bucket, class string, bytes int64) {
	if bucket == "" {
		bucket = "unknown"
	}
	if class == "" {
		class = "STANDARD"
	}
	StorageClassBytes.WithLabelValues(class, bucket).Set(float64(bytes))
}

func (BucketStatsObserver) SetStorageClassObjects(bucket, class string, objects int64) {
	if bucket == "" {
		bucket = "unknown"
	}
	if class == "" {
		class = "STANDARD"
	}
	StorageClassObjects.WithLabelValues(class, bucket).Set(float64(objects))
}

// ResetBucketClass drops every (class, bucket) series for bucket so a
// freshly dropped-from-top-N bucket does not linger as stale data.
func (BucketStatsObserver) ResetBucketClass(bucket string) {
	if bucket == "" {
		return
	}
	StorageClassBytes.DeletePartialMatch(prometheus.Labels{"bucket": bucket})
	StorageClassObjects.DeletePartialMatch(prometheus.Labels{"bucket": bucket})
}

// RebalanceObserver implements the rebalance.Metrics interface. The
// rebalance worker bumps the planned_moves_total counter per chunk-
// move emitted by the plan-scan loop (US-003); mover side counters
// land in US-004/US-005.
type RebalanceObserver struct{}

func (RebalanceObserver) IncPlannedMove(bucket string) {
	if bucket == "" {
		bucket = "unknown"
	}
	RebalancePlannedMovesTotal.WithLabelValues(bucket).Inc()
}

func (RebalanceObserver) IncBytesMoved(from, to string, bytes int64) {
	if bytes <= 0 {
		return
	}
	if from == "" {
		from = "unknown"
	}
	if to == "" {
		to = "unknown"
	}
	RebalanceBytesMovedTotal.WithLabelValues(from, to).Add(float64(bytes))
}

func (RebalanceObserver) IncChunksMoved(from, to, bucket string) {
	if from == "" {
		from = "unknown"
	}
	if to == "" {
		to = "unknown"
	}
	if bucket == "" {
		bucket = "unknown"
	}
	RebalanceChunksMovedTotal.WithLabelValues(from, to, bucket).Inc()
}

func (RebalanceObserver) IncCASConflict(bucket string) {
	if bucket == "" {
		bucket = "unknown"
	}
	RebalanceCASConflictsTotal.WithLabelValues(bucket).Inc()
}

func (RebalanceObserver) IncRefused(reason, target string) {
	if reason == "" {
		reason = "unknown"
	}
	if target == "" {
		target = "unknown"
	}
	RebalanceRefusedTotal.WithLabelValues(reason, target).Inc()
}

// AuditStreamObserver implements the auditstream.MetricsSink interface. The
// gauge tracks the in-process subscriber count for /admin/v1/audit/stream.
type AuditStreamObserver struct{}

func (AuditStreamObserver) SetSubscribers(n int) {
	AuditStreamSubscribers.Set(float64(n))
}

// OTelRingbufObserver implements the ringbuf.MetricsSink interface. Used by
// the otel package wiring so the prometheus dependency stays in cmd-layer
// adapters.
type OTelRingbufObserver struct{}

func (OTelRingbufObserver) SetTraces(n int) { OTelRingbufTraces.Set(float64(n)) }
func (OTelRingbufObserver) IncEvicted()     { OTelRingbufEvicted.Inc() }

// ReplicationObserver extends MetricsObserver with SetQueueDepth so the
// replicator can publish per-rule pending counts.
type ReplicationObserver struct{}

func (ReplicationObserver) ObserveLag(ruleID string, lag float64) {
	if ruleID == "" {
		ruleID = "unknown"
	}
	ReplicationLagSeconds.WithLabelValues(ruleID).Observe(lag)
}

func (ReplicationObserver) IncCompleted(ruleID string) {
	if ruleID == "" {
		ruleID = "unknown"
	}
	ReplicationCompleted.WithLabelValues(ruleID).Inc()
}

func (ReplicationObserver) IncFailed(ruleID string) {
	if ruleID == "" {
		ruleID = "unknown"
	}
	ReplicationFailed.WithLabelValues(ruleID).Inc()
}

func (ReplicationObserver) SetQueueDepth(ruleID string, depth int) {
	if ruleID == "" {
		ruleID = "unknown"
	}
	ReplicationQueueDepth.WithLabelValues(ruleID).Set(float64(depth))
}

// SetQueueAge publishes the oldest pending row age (seconds) for the given
// source bucket. Backs the per-bucket Replication tab (US-014). Empty bucket
// collapses to "unknown" so a missing label never silently drops the sample.
func (ReplicationObserver) SetQueueAge(bucket string, ageSeconds float64) {
	if bucket == "" {
		bucket = "unknown"
	}
	ReplicationQueueAge.WithLabelValues(bucket).Set(ageSeconds)
}
