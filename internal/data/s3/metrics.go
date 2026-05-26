package s3

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws/retry"
	smithy "github.com/aws/smithy-go"
	"github.com/aws/smithy-go/middleware"
	"github.com/prometheus/client_golang/prometheus"
)

// APIMetrics is the cmd-layer Prom sink for the per-cluster AWS SDK
// counters (US-001 cycle B prod-observability). The S3 backend stays free
// of internal/metrics imports — wired via Config.APIMetrics. Nil disables
// (no-op middleware).
type APIMetrics interface {
	// IncAPICall bumps strata_data_s3_api_calls_total{cluster, operation,
	// outcome}. outcome ∈ {success, error, throttled}. Throttled is bumped
	// alongside error when the terminal failure matches a known AWS
	// throttle error code.
	IncAPICall(cluster, operation, outcome string)
	// IncThrottled bumps strata_data_s3_throttled_total{cluster,
	// operation} on every observed throttle response (terminal or retried).
	IncThrottled(cluster, operation string)
}

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
// (serialize → retry → send → deserialize). cluster + apiMetrics route
// the per-cluster counters added in US-001 cycle B prod-observability;
// empty cluster + nil apiMetrics keeps the existing single-cluster +
// no-Prom behaviour intact for the legacy test fixtures.
func instrumentStack(cluster string, apiMetrics APIMetrics) func(*middleware.Stack) error {
	return func(stack *middleware.Stack) error {
		return stack.Initialize.Add(metricsObserver{cluster: cluster, apiMetrics: apiMetrics}, middleware.Before)
	}
}

// metricsObserver records latency, terminal status, and retry count for
// each S3 op. Reads middleware.GetOperationName for the SDK op name and
// retry.GetAttemptResults from the response metadata for the attempt
// count — preferred over wrapping the retry middleware directly.
type metricsObserver struct {
	cluster    string
	apiMetrics APIMetrics
}

func (metricsObserver) ID() string { return observerMiddlewareID }

func (o metricsObserver) HandleInitialize(ctx context.Context, in middleware.InitializeInput, next middleware.InitializeHandler) (middleware.InitializeOutput, middleware.Metadata, error) {
	start := time.Now()
	out, md, err := next.HandleInitialize(ctx, in)

	opName := middleware.GetOperationName(ctx)
	label, ok := opNameToLabel[opName]
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

	// US-001 cycle B prod-observability — per-cluster outcome split. The
	// outcome label maps {ok, retried} → success, error → error (or
	// throttled when the failure carries an AWS throttle error code).
	// Throttled retries that ultimately succeeded still bump the throttle
	// counter so operators can see backend pressure even without a
	// terminal failure.
	if o.apiMetrics != nil {
		outcome := "success"
		switch {
		case err != nil && isThrottle(err):
			outcome = "throttled"
			o.apiMetrics.IncThrottled(o.cluster, label)
		case err != nil:
			outcome = "error"
		}
		if err == nil && observedThrottle(md) {
			// Terminal success after one or more throttle retries.
			o.apiMetrics.IncThrottled(o.cluster, label)
		}
		o.apiMetrics.IncAPICall(o.cluster, label, outcome)
	}
	return out, md, err
}

// isThrottle inspects err for an AWS throttling-class API error code.
// Mirrors the smithy retry classifier's ThrottlingException short-list
// without pulling in the full retry package — keeps the throttle counter
// honest for both terminal failures and intermediate retry attempts.
func isThrottle(err error) bool {
	if err == nil {
		return false
	}
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	switch apiErr.ErrorCode() {
	case "ThrottlingException", "Throttling", "SlowDown",
		"RequestLimitExceeded", "RequestThrottled", "TooManyRequestsException",
		"ProvisionedThroughputExceededException", "BandwidthLimitExceeded":
		return true
	}
	return false
}

// observedThrottle returns true when any retry attempt observed an AWS
// throttle error code, even if the terminal attempt succeeded. SDK
// records per-attempt errors in retry.GetAttemptResults — walk them.
func observedThrottle(md middleware.Metadata) bool {
	results, present := retry.GetAttemptResults(md)
	if !present {
		return false
	}
	for _, r := range results.Results {
		if r.Err != nil && isThrottle(r.Err) {
			return true
		}
	}
	return false
}
