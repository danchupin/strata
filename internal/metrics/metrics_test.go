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
	ch := make(chan *prometheus.Desc, 8)
	go func() {
		HTTPDuration.Describe(ch)
		CassandraQueryDuration.Describe(ch)
		close(ch)
	}()
	for d := range ch {
		if strings.Contains(d.String(), `"`+name+`"`) {
			return true
		}
	}
	return false
}
