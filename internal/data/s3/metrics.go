package s3

import (
	"context"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/smithy-go/middleware"
	"github.com/prometheus/client_golang/prometheus"
)

// US-007 metric collectors. Per-op latency + error rates for the S3 data
// backend so operators can alert on backend regressions. Observers
// piggyback on the SDK's middleware.Stack — a single Initialize-step
// middleware registered once at client init wraps every operation.
//
// Status semantics:
//
//	ok       — terminal success on the first attempt
//	retried  — terminal success that required at least one retry
//	error    — terminal failure (after the SDK retry budget was exhausted)
var (
	opDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "strata_data_s3_backend_op_duration_seconds",
		Help:    "Per-op latency for the S3 data backend, partitioned by op + terminal status.",
		Buckets: prometheus.ExponentialBuckets(0.005, 2, 14),
	}, []string{"op", "status"})

	opTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "strata_data_s3_backend_op_total",
		Help: "Total S3-backend ops, partitioned by op + terminal status.",
	}, []string{"op", "status"})

	retryTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "strata_data_s3_backend_retry_total",
		Help: "Adaptive-retry attempts (excluding the initial try) per op, surfacing backend retry pressure.",
	}, []string{"op"})

	registerOnce sync.Once
)

// RegisterMetrics registers the S3-backend Prometheus collectors on the
// default registry. Idempotent — safe to call from each entry-point's
// boot path (cmd/strata-gateway, cmd/strata-gc, cmd/strata-lifecycle).
func RegisterMetrics() {
	registerOnce.Do(func() {
		prometheus.MustRegister(opDuration, opTotal, retryTotal)
	})
}

// opNameToLabel maps the SDK operation name (set by smithy at request
// boot) to the metric label used by the AC. Operations not in this map
// (e.g. ListObjectVersions issued by tests, HeadBucket from the SDK's
// region-discovery middleware) skip metric observation — keeping the
// label cardinality bounded to the eight ops the AC pins.
var opNameToLabel = map[string]string{
	"PutObject":               "put",
	"GetObject":               "get",
	"DeleteObject":            "delete",
	"DeleteObjects":           "batch_delete",
	"CreateMultipartUpload":   "multipart_init",
	"UploadPart":              "multipart_part",
	"CompleteMultipartUpload": "multipart_complete",
	"AbortMultipartUpload":    "multipart_abort",
}

// observerMiddlewareID is the smithy middleware ID for the metrics
// observer. Stable string so the registration is idempotent across
// calls to instrumentStack on a shared APIOptions slice.
const observerMiddlewareID = "StrataS3MetricsObserver"

// instrumentStack adds the metrics-observer middleware at the head of
// the Initialize step so it brackets the entire operation lifecycle
// (serialize → retry → send → deserialize).
func instrumentStack(stack *middleware.Stack) error {
	return stack.Initialize.Add(metricsObserver{}, middleware.Before)
}

// metricsObserver records latency, terminal status, and retry count for
// each S3 op. Reads middleware.GetOperationName for the SDK op name and
// retry.GetAttemptResults from the response metadata for the attempt
// count — preferred over wrapping the retry middleware directly.
type metricsObserver struct{}

func (metricsObserver) ID() string { return observerMiddlewareID }

func (metricsObserver) HandleInitialize(ctx context.Context, in middleware.InitializeInput, next middleware.InitializeHandler) (middleware.InitializeOutput, middleware.Metadata, error) {
	start := time.Now()
	out, md, err := next.HandleInitialize(ctx, in)

	label, ok := opNameToLabel[middleware.GetOperationName(ctx)]
	if !ok {
		return out, md, err
	}

	attempts := 1
	if results, present := retry.GetAttemptResults(md); present && len(results.Results) > 0 {
		attempts = len(results.Results)
	}

	status := "ok"
	switch {
	case err != nil:
		status = "error"
	case attempts > 1:
		status = "retried"
	}

	opDuration.WithLabelValues(label, status).Observe(time.Since(start).Seconds())
	opTotal.WithLabelValues(label, status).Inc()
	if attempts > 1 {
		retryTotal.WithLabelValues(label).Add(float64(attempts - 1))
	}
	return out, md, err
}
