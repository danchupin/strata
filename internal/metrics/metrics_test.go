package metrics

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func TestTemplatePath(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", "/"},
		{"/", "/"},
		{"/metrics", "/metrics"},
		{"/healthz", "/healthz"},
		{"/readyz", "/readyz"},
		{"/bkt", "/{bucket}"},
		{"/bkt/", "/{bucket}/{key}"},
		{"/bkt/key.txt", "/{bucket}/{key}"},
		{"/bkt/path/with/slashes", "/{bucket}/{key}"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := TemplatePath(tc.in); got != tc.want {
				t.Fatalf("TemplatePath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestObserveHTTPLabelsHistogram(t *testing.T) {
	HTTPDuration.Reset()
	HTTPRequests.Reset()

	h := ObserveHTTP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/bkt/some/key", nil)
	h.ServeHTTP(rec, req)

	count := histogramCount(t, HTTPDuration.WithLabelValues("PUT", "/{bucket}/{key}", "201"))
	if count != 1 {
		t.Fatalf("expected 1 observation for PUT /{bucket}/{key} 201, got %d", count)
	}
}

func TestObserveHTTPDefaultStatusIs200(t *testing.T) {
	HTTPDuration.Reset()
	h := ObserveHTTP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// no WriteHeader -> default 200
		_, _ = w.Write([]byte("ok"))
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/bkt", nil)
	h.ServeHTTP(rec, req)
	if c := histogramCount(t, HTTPDuration.WithLabelValues("GET", "/{bucket}", "200")); c != 1 {
		t.Fatalf("expected 1 observation for GET /{bucket} 200, got %d", c)
	}
}

func TestCassandraObserverRecords(t *testing.T) {
	CassandraQueryDuration.Reset()
	var obs CassandraObserver
	obs.ObserveQuery("buckets", "SELECT", 50*time.Millisecond, nil)
	if c := histogramCount(t, CassandraQueryDuration.WithLabelValues("buckets", "SELECT")); c != 1 {
		t.Fatalf("expected 1 observation, got %d", c)
	}
	// Empty inputs collapse onto safe defaults.
	obs.ObserveQuery("", "", 0, errors.New("boom"))
	if c := histogramCount(t, CassandraQueryDuration.WithLabelValues("unknown", "UNKNOWN")); c != 1 {
		t.Fatalf("expected 1 observation under (unknown,UNKNOWN), got %d", c)
	}
}

func histogramCount(t *testing.T, h prometheus.Observer) uint64 {
	t.Helper()
	collector, ok := h.(prometheus.Metric)
	if !ok {
		t.Fatalf("observer is not a Metric: %T", h)
	}
	var m dto.Metric
	if err := collector.Write(&m); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if m.Histogram == nil {
		t.Fatal("metric is not a histogram")
	}
	return m.Histogram.GetSampleCount()
}

func TestRegisterIdempotentMetricsExposedNames(t *testing.T) {
	// Ensure the histogram metric names match what dashboards expect — guards
	// accidental rename. Skips registration (handled by Register() in main).
	for _, want := range []string{
		"strata_http_request_duration_seconds",
		"strata_cassandra_query_duration_seconds",
	} {
		if !metricNamePresent(want) {
			t.Errorf("expected metric %q to be defined", want)
		}
	}
}

func metricNamePresent(name string) bool {
	ch := make(chan *prometheus.Desc, 32)
	go func() {
		HTTPDuration.Describe(ch)
		CassandraQueryDuration.Describe(ch)
		RADOSOpDuration.Describe(ch)
		GCQueueDepth.Describe(ch)
		MultipartActive.Describe(ch)
		BucketBytes.Describe(ch)
		LifecycleTickTotal.Describe(ch)
		NotifyDeliveryTotal.Describe(ch)
		ReplicationQueueDepth.Describe(ch)
		close(ch)
	}()
	for d := range ch {
		if strings.Contains(d.String(), `"`+name+`"`) {
			return true
		}
	}
	return false
}

func TestRADOSObserverRecords(t *testing.T) {
	RADOSOpDuration.Reset()
	var obs RADOSObserver
	obs.ObserveOp("rgw.data", "put", 5*time.Millisecond, nil)
	if c := histogramCount(t, RADOSOpDuration.WithLabelValues("rgw.data", "put")); c != 1 {
		t.Fatalf("expected 1 obs for (rgw.data,put), got %d", c)
	}
	obs.ObserveOp("", "", 0, nil)
	if c := histogramCount(t, RADOSOpDuration.WithLabelValues("unknown", "unknown")); c != 1 {
		t.Fatalf("expected 1 obs for (unknown,unknown), got %d", c)
	}
}

func TestGCObserverSetsDepth(t *testing.T) {
	GCQueueDepth.Reset()
	var obs GCObserver
	obs.SetQueueDepth("eu", 42)
	if v := gaugeValue(t, GCQueueDepth.WithLabelValues("eu")); v != 42 {
		t.Fatalf("expected 42, got %v", v)
	}
	obs.SetQueueDepth("", 7)
	if v := gaugeValue(t, GCQueueDepth.WithLabelValues("default")); v != 7 {
		t.Fatalf("expected 7 under default, got %v", v)
	}
}

func TestNotifyObserverIncrements(t *testing.T) {
	NotifyDeliveryTotal.Reset()
	var obs NotifyObserver
	obs.IncDelivery("webhook:wh1", "success")
	obs.IncDelivery("webhook:wh1", "success")
	obs.IncDelivery("sqs:q1", "failure")
	obs.IncDelivery("", "")
	if c := counterValue(t, NotifyDeliveryTotal.WithLabelValues("webhook:wh1", "success")); c != 2 {
		t.Fatalf("expected 2 success, got %v", c)
	}
	if c := counterValue(t, NotifyDeliveryTotal.WithLabelValues("sqs:q1", "failure")); c != 1 {
		t.Fatalf("expected 1 failure, got %v", c)
	}
	if c := counterValue(t, NotifyDeliveryTotal.WithLabelValues("unknown", "unknown")); c != 1 {
		t.Fatalf("expected 1 unknown/unknown, got %v", c)
	}
}

func TestLifecycleObserverCounter(t *testing.T) {
	LifecycleTickTotal.Reset()
	var obs LifecycleObserver
	obs.IncTick("transition", "success")
	obs.IncTick("transition", "success")
	obs.IncTick("expire", "error")
	if c := counterValue(t, LifecycleTickTotal.WithLabelValues("transition", "success")); c != 2 {
		t.Fatalf("expected 2 transition/success, got %v", c)
	}
	if c := counterValue(t, LifecycleTickTotal.WithLabelValues("expire", "error")); c != 1 {
		t.Fatalf("expected 1 expire/error, got %v", c)
	}
}

func TestBucketStatsObserverSetsGauge(t *testing.T) {
	BucketBytes.Reset()
	var obs BucketStatsObserver
	obs.SetBucketBytes("bkt", "STANDARD", 1024)
	obs.SetBucketBytes("bkt", "GLACIER", 4096)
	obs.SetBucketBytes("", "", 8)
	if v := gaugeValue(t, BucketBytes.WithLabelValues("bkt", "STANDARD")); v != 1024 {
		t.Fatalf("expected 1024, got %v", v)
	}
	if v := gaugeValue(t, BucketBytes.WithLabelValues("bkt", "GLACIER")); v != 4096 {
		t.Fatalf("expected 4096, got %v", v)
	}
	if v := gaugeValue(t, BucketBytes.WithLabelValues("unknown", "STANDARD")); v != 8 {
		t.Fatalf("expected 8 under default labels, got %v", v)
	}
}

func TestReplicationObserverFullSurface(t *testing.T) {
	ReplicationLagSeconds.Reset()
	ReplicationCompleted.Reset()
	ReplicationFailed.Reset()
	ReplicationQueueDepth.Reset()
	var obs ReplicationObserver
	obs.ObserveLag("r1", 12.5)
	obs.IncCompleted("r1")
	obs.IncFailed("r1")
	obs.SetQueueDepth("r1", 9)
	if c := histogramCount(t, ReplicationLagSeconds.WithLabelValues("r1")); c != 1 {
		t.Fatalf("expected 1 lag obs, got %v", c)
	}
	if c := counterValue(t, ReplicationCompleted.WithLabelValues("r1")); c != 1 {
		t.Fatalf("expected 1 completed, got %v", c)
	}
	if c := counterValue(t, ReplicationFailed.WithLabelValues("r1")); c != 1 {
		t.Fatalf("expected 1 failed, got %v", c)
	}
	if v := gaugeValue(t, ReplicationQueueDepth.WithLabelValues("r1")); v != 9 {
		t.Fatalf("expected depth 9, got %v", v)
	}
}

func TestExposedMetricNames(t *testing.T) {
	for _, want := range []string{
		"strata_rados_op_duration_seconds",
		"strata_gc_queue_depth",
		"strata_multipart_active",
		"strata_bucket_bytes",
		"strata_lifecycle_tick_total",
		"strata_notify_delivery_total",
		"strata_replication_queue_depth",
	} {
		if !metricNamePresent(want) {
			t.Errorf("expected metric %q to be defined", want)
		}
	}
}

func TestBucketShardObserverSetsAndResets(t *testing.T) {
	BucketShardBytes.Reset()
	BucketShardObjects.Reset()
	var obs BucketStatsObserver
	obs.SetBucketShardBytes("bkt", 0, 1000)
	obs.SetBucketShardBytes("bkt", 1, 2000)
	obs.SetBucketShardObjects("bkt", 0, 5)
	obs.SetBucketShardObjects("bkt", 1, 7)
	obs.SetBucketShardBytes("other", 0, 99)

	if v := gaugeValue(t, BucketShardBytes.WithLabelValues("bkt", "0")); v != 1000 {
		t.Fatalf("bkt|0 bytes: %v", v)
	}
	if v := gaugeValue(t, BucketShardBytes.WithLabelValues("bkt", "1")); v != 2000 {
		t.Fatalf("bkt|1 bytes: %v", v)
	}
	if v := gaugeValue(t, BucketShardObjects.WithLabelValues("bkt", "0")); v != 5 {
		t.Fatalf("bkt|0 objects: %v", v)
	}

	// Reset wipes only the requested bucket's series.
	obs.ResetBucketShard("bkt")
	for _, shard := range []string{"0", "1"} {
		ch := make(chan prometheus.Metric, 4)
		BucketShardBytes.Collect(ch)
		close(ch)
		for m := range ch {
			var dm dto.Metric
			if err := m.Write(&dm); err != nil {
				t.Fatalf("Write: %v", err)
			}
			gotBucket, gotShard := "", ""
			for _, lp := range dm.Label {
				switch lp.GetName() {
				case "bucket":
					gotBucket = lp.GetValue()
				case "shard":
					gotShard = lp.GetValue()
				}
			}
			if gotBucket == "bkt" && gotShard == shard {
				t.Fatalf("expected bkt|%s wiped after reset; still present", shard)
			}
		}
	}
	if v := gaugeValue(t, BucketShardBytes.WithLabelValues("other", "0")); v != 99 {
		t.Fatalf("unrelated series clobbered: %v", v)
	}
}

// TestHandlerExposesProcessAndGoMetrics asserts the prometheus default
// registerer exposes the standard process collector + go collector metrics
// the per-node drilldown (US-011) reads. client_golang auto-registers them
// in init(); this test guards against a future custom registry that omits
// them.
func TestHandlerExposesProcessAndGoMetrics(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"process_cpu_seconds_total",
		"process_resident_memory_bytes",
		"process_open_fds",
		"go_goroutines",
		"go_gc_duration_seconds",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metric %q absent from /metrics output", want)
		}
	}
}

func gaugeValue(t *testing.T, g prometheus.Gauge) float64 {
	t.Helper()
	var m dto.Metric
	if err := g.Write(&m); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if m.Gauge == nil {
		t.Fatal("metric is not a gauge")
	}
	return m.Gauge.GetValue()
}

func counterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if m.Counter == nil {
		t.Fatal("metric is not a counter")
	}
	return m.Counter.GetValue()
}
