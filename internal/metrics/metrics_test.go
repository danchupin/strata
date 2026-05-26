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

func TestBucketLabel(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", "_root"},
		{"/", "_root"},
		{"/admin/v1/cluster/nodes", "_admin"},
		{"/metrics", "_admin"},
		{"/healthz", "_admin"},
		{"/readyz", "_admin"},
		{"/console/index.html", "_admin"},
		{"/lab-test", "lab-test"},
		{"/lab-test/", "lab-test"},
		{"/lab-test/file.txt", "lab-test"},
		{"/lab-test/path/with/slashes", "lab-test"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := bucketLabel(tc.in); got != tc.want {
				t.Fatalf("bucketLabel(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestObserveHTTPCounterLabels regression-tests the bucket / access_key
// labels added to strata_http_requests_total. Without these, PromQL
// `sum by (bucket)` (Hot Buckets, Top Buckets on Overview) and
// `sum by (access_key)` (Consumers leaderboard) returned an empty matrix
// even when Prometheus was scraping correctly.
func TestObserveHTTPCounterLabels(t *testing.T) {
	HTTPRequests.Reset()
	t.Cleanup(func() { HTTPMetricsLabeler = nil })
	HTTPMetricsLabeler = func(r *http.Request) string { return "alice" }

	h := ObserveHTTP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/lab-test/file.txt", nil)
	h.ServeHTTP(rec, req)

	if got := counterValue(t, HTTPRequests.WithLabelValues("PUT", "200", "lab-test", "alice")); got != 1 {
		t.Fatalf("PUT /lab-test/file.txt expected counter=1 with bucket=lab-test, ak=alice, got %v", got)
	}
}

// TestObserveHTTPCounterAnonymous covers the nil-labeler path (boot-time)
// and the anonymous-request path (auth.FromContext returns IsAnonymous).
func TestObserveHTTPCounterAnonymous(t *testing.T) {
	HTTPRequests.Reset()
	HTTPMetricsLabeler = nil // boot-time / never wired

	h := ObserveHTTP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/cluster/nodes", nil)
	h.ServeHTTP(rec, req)

	if got := counterValue(t, HTTPRequests.WithLabelValues("GET", "200", "_admin", "_anon")); got != 1 {
		t.Fatalf("nil labeler should yield ak=_anon, got counter=%v", got)
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

// TestUS001MetricGapFillRegistered asserts the 9 metrics added in US-001
// cycle B prod-observability are reachable by Describe() and carry a
// non-empty Help string. Future US-002+ alerts + dashboards reference these
// names verbatim — drift here breaks the alert/dashboard drift-lint
// (US-010). Also covers the 3 rebalance categorisation gauges folded into
// US-001 for US-007's cluster dashboard drain-progress panel.
func TestUS001MetricGapFillRegistered(t *testing.T) {
	required := map[string]prometheus.Collector{
		"strata_heartbeat_last_write_timestamp":              HeartbeatLastWriteTimestamp,
		"strata_rados_cluster_object_count":                  RADOSClusterObjectCount,
		"strata_rados_cluster_bytes_used":                    RADOSClusterBytesUsed,
		"strata_bucket_quota_bytes":                          BucketQuotaBytes,
		"strata_tikv_pessimistic_txn_total":                  TiKVPessimisticTxnTotal,
		"strata_data_s3_api_calls_total":                     DataS3APICallsTotal,
		"strata_data_s3_throttled_total":                     DataS3ThrottledTotal,
		"strata_inventory_objects_total":                     InventoryObjectsTotal,
		"strata_worker_leader_events_total":                  WorkerLeaderEventsTotal,
		"strata_rebalance_migratable_chunks_total":           RebalanceMigratableChunksTotal,
		"strata_rebalance_stuck_single_policy_chunks_total":  RebalanceStuckSinglePolicyChunksTotal,
		"strata_rebalance_stuck_no_policy_chunks_total":      RebalanceStuckNoPolicyChunksTotal,
	}
	for name, c := range required {
		ch := make(chan *prometheus.Desc, 4)
		go func() {
			c.Describe(ch)
			close(ch)
		}()
		var seenName, seenHelp bool
		for d := range ch {
			s := d.String()
			if strings.Contains(s, `"`+name+`"`) {
				seenName = true
			}
			// Desc.String() shape: `Desc{fqName: "X", help: "Y", ...}`.
			if strings.Contains(s, `help: ""`) {
				continue
			}
			seenHelp = true
		}
		if !seenName {
			t.Errorf("metric %q not exposed by its Describe()", name)
		}
		if !seenHelp {
			t.Errorf("metric %q missing Help string", name)
		}
	}
}

// TestUS001ObserverAdapters smoke-tests the new observer methods so a
// missing wiring (e.g. forgotten registration) surfaces here rather than
// in the smoke. Each method bumps a series — assert the underlying value
// landed.
func TestUS001ObserverAdapters(t *testing.T) {
	HeartbeatLastWriteTimestamp.Set(0)
	HeartbeatObserver{}.SetLastWriteTimestamp(1234567890)
	if v := gaugeValue(t, HeartbeatLastWriteTimestamp); v != 1234567890 {
		t.Fatalf("heartbeat ts: %v", v)
	}

	RADOSClusterObjectCount.Reset()
	RADOSClusterBytesUsed.Reset()
	RADOSObserver{}.SetClusterObjectCount("ceph-a", 42)
	RADOSObserver{}.SetClusterBytesUsed("ceph-a", 9999)
	if v := gaugeValue(t, RADOSClusterObjectCount.WithLabelValues("ceph-a")); v != 42 {
		t.Fatalf("rados objects: %v", v)
	}
	if v := gaugeValue(t, RADOSClusterBytesUsed.WithLabelValues("ceph-a")); v != 9999 {
		t.Fatalf("rados bytes: %v", v)
	}

	BucketQuotaBytes.Reset()
	BucketStatsObserver{}.SetBucketQuotaBytes("bkt", 1024)
	if v := gaugeValue(t, BucketQuotaBytes.WithLabelValues("bkt")); v != 1024 {
		t.Fatalf("quota: %v", v)
	}

	TiKVPessimisticTxnTotal.Reset()
	TiKVObserver{}.IncPessimisticTxn("CreateBucket", "commit")
	if v := counterValue(t, TiKVPessimisticTxnTotal.WithLabelValues("CreateBucket", "commit")); v != 1 {
		t.Fatalf("tikv pessimistic: %v", v)
	}

	DataS3APICallsTotal.Reset()
	DataS3ThrottledTotal.Reset()
	S3APIObserver{}.IncAPICall("us-east-1", "put", "success")
	S3APIObserver{}.IncThrottled("us-east-1", "put")
	if v := counterValue(t, DataS3APICallsTotal.WithLabelValues("us-east-1", "put", "success")); v != 1 {
		t.Fatalf("s3 api: %v", v)
	}
	if v := counterValue(t, DataS3ThrottledTotal.WithLabelValues("us-east-1", "put")); v != 1 {
		t.Fatalf("s3 throttled: %v", v)
	}

	InventoryObjectsTotal.Reset()
	InventoryObserver{}.SetObjectsTotal("bkt", "cfg1", 9001)
	if v := gaugeValue(t, InventoryObjectsTotal.WithLabelValues("bkt", "cfg1")); v != 9001 {
		t.Fatalf("inventory: %v", v)
	}

	WorkerLeaderEventsTotal.Reset()
	IncLeaderEvent("gc", "acquired")
	IncLeaderEvent("gc", "released")
	if v := counterValue(t, WorkerLeaderEventsTotal.WithLabelValues("gc", "acquired")); v != 1 {
		t.Fatalf("leader acquired: %v", v)
	}
	if v := counterValue(t, WorkerLeaderEventsTotal.WithLabelValues("gc", "released")); v != 1 {
		t.Fatalf("leader released: %v", v)
	}

	RebalanceMigratableChunksTotal.Reset()
	RebalanceStuckSinglePolicyChunksTotal.Reset()
	RebalanceStuckNoPolicyChunksTotal.Reset()
	var ro RebalanceObserver
	ro.SetMigratableChunks("c1", 7)
	ro.SetStuckSinglePolicyChunks("c1", 3)
	ro.SetStuckNoPolicyChunks("c1", 1)
	if v := gaugeValue(t, RebalanceMigratableChunksTotal.WithLabelValues("c1")); v != 7 {
		t.Fatalf("migratable: %v", v)
	}
	if v := gaugeValue(t, RebalanceStuckSinglePolicyChunksTotal.WithLabelValues("c1")); v != 3 {
		t.Fatalf("stuck single: %v", v)
	}
	if v := gaugeValue(t, RebalanceStuckNoPolicyChunksTotal.WithLabelValues("c1")); v != 1 {
		t.Fatalf("stuck nopolicy: %v", v)
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
