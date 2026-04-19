package metrics

import (
	"net/http"
	"strconv"
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
			Help:    "Latency of HTTP requests served by the gateway.",
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
		},
		[]string{"method"},
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
)

func Register() {
	prometheus.MustRegister(HTTPRequests, HTTPDuration, GCEnqueued, GCProcessed, LifecycleTransitions, LifecycleExpirations)
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
		HTTPRequests.WithLabelValues(r.Method, strconv.Itoa(rw.status)).Inc()
		HTTPDuration.WithLabelValues(r.Method).Observe(time.Since(start).Seconds())
	})
}
