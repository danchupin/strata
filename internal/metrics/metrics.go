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
			Help: "Total HTTP requests served by the gateway, partitioned by method and response code.",
		},
		[]string{"method", "code"},
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
			Help: "Number of panics caught and recovered by the worker supervisor, per worker name.",
		},
		[]string{"worker"},
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
)

func Register() {
	prometheus.MustRegister(
		HTTPRequests, HTTPDuration,
		CassandraQueryDuration,
		GCEnqueued, GCProcessed,
		LifecycleTransitions, LifecycleExpirations,
		ReplicationLagSeconds, ReplicationCompleted, ReplicationFailed,
		ReplicationQueueDepth,
		RADOSOpDuration,
		GCQueueDepth,
		MultipartActive,
		BucketBytes,
		LifecycleTickTotal,
		NotifyDeliveryTotal,
		WorkerPanicTotal,
		MetaTikvAuditSweepDeleted,
		AuditStreamSubscribers,
		OTelRingbufTraces,
		OTelRingbufEvicted,
		CassandraLWTConflictsTotal,
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

func ObserveHTTP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &wrappedWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r)
		code := strconv.Itoa(rw.status)
		HTTPRequests.WithLabelValues(r.Method, code).Inc()
		HTTPDuration.WithLabelValues(r.Method, TemplatePath(r.URL.Path), code).Observe(time.Since(start).Seconds())
	})
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
