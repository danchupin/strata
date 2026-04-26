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
)

func Register() {
	prometheus.MustRegister(
		HTTPRequests, HTTPDuration,
		CassandraQueryDuration,
		GCEnqueued, GCProcessed,
		LifecycleTransitions, LifecycleExpirations,
		ReplicationLagSeconds, ReplicationCompleted, ReplicationFailed,
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
