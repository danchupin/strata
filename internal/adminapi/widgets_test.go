package adminapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/promclient"
	"github.com/google/uuid"
)

func seedBucket(t *testing.T, store meta.Store, name string, sizes []int64) {
	t.Helper()
	ctx := context.Background()
	if _, err := store.CreateBucket(ctx, name, "owner-"+name, "STANDARD"); err != nil {
		t.Fatalf("create bucket %s: %v", name, err)
	}
	b, err := store.GetBucket(ctx, name)
	if err != nil {
		t.Fatalf("get bucket %s: %v", name, err)
	}
	for i, sz := range sizes {
		if err := store.PutObject(ctx, &meta.Object{
			BucketID: b.ID,
			Key:      keyN(i),
			Size:     sz,
			ETag:     "deadbeef",
			IsLatest: true,
			Manifest: &data.Manifest{},
		}, false); err != nil {
			t.Fatalf("put object %s/%d: %v", name, i, err)
		}
	}
	_ = uuid.UUID{}
}

func keyN(i int) string {
	return "obj-" + string(rune('a'+i))
}

func TestBucketsTopBySizeWithoutProm(t *testing.T) {
	s := newTestServer()
	seedBucket(t, s.Meta, "alpha", []int64{1024, 2048})
	seedBucket(t, s.Meta, "beta", []int64{10_000})
	seedBucket(t, s.Meta, "gamma", nil)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/buckets/top?by=size&limit=2", nil)
	s.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got BucketsTopResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.MetricsAvailable {
		t.Error("MetricsAvailable: want false (no Prom configured)")
	}
	if len(got.Buckets) != 2 {
		t.Fatalf("len=%d want 2", len(got.Buckets))
	}
	if got.Buckets[0].Name != "beta" || got.Buckets[0].SizeBytes != 10_000 {
		t.Errorf("first=%+v want beta/10000", got.Buckets[0])
	}
	if got.Buckets[1].Name != "alpha" || got.Buckets[1].SizeBytes != 3072 || got.Buckets[1].ObjectCount != 2 {
		t.Errorf("second=%+v want alpha/3072/2", got.Buckets[1])
	}
}

func TestBucketsTopByRequestsUsesPromCounts(t *testing.T) {
	s := newTestServer()
	seedBucket(t, s.Meta, "alpha", []int64{1})
	seedBucket(t, s.Meta, "beta", []int64{1})
	seedBucket(t, s.Meta, "gamma", []int64{1})

	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[
			{"metric":{"bucket":"alpha"},"value":[1700000000.0,"5"]},
			{"metric":{"bucket":"beta"},"value":[1700000000.0,"50"]},
			{"metric":{"bucket":"gamma"},"value":[1700000000.0,"500"]}
		]}}`))
	}))
	defer prom.Close()
	s.Prom = promclient.New(prom.URL)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/buckets/top?by=requests", nil)
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got BucketsTopResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.MetricsAvailable {
		t.Error("MetricsAvailable: want true")
	}
	if len(got.Buckets) != 3 {
		t.Fatalf("len=%d", len(got.Buckets))
	}
	if got.Buckets[0].Name != "gamma" || got.Buckets[0].RequestCount24h != 500 {
		t.Errorf("first=%+v want gamma/500", got.Buckets[0])
	}
	if got.Buckets[2].Name != "alpha" || got.Buckets[2].RequestCount24h != 5 {
		t.Errorf("last=%+v want alpha/5", got.Buckets[2])
	}
}

func TestBucketsTopRejectsBadByParam(t *testing.T) {
	s := newTestServer()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/buckets/top?by=alphabetical", nil)
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400", rr.Code)
	}
}

func TestConsumersTopWithoutPromReturnsEmpty(t *testing.T) {
	s := newTestServer()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/consumers/top", nil)
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	var got ConsumersTopResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.MetricsAvailable {
		t.Error("MetricsAvailable: want false (no Prom configured)")
	}
	if got.Consumers == nil {
		t.Error("Consumers must be empty array not nil")
	}
}

func TestConsumersTopByRequestsSortsAndJoinsOwner(t *testing.T) {
	s := newTestServer()
	s.Creds = auth.NewStaticStore(map[string]*auth.Credential{
		"AKIATESTAAA": {AccessKey: "AKIATESTAAA", Owner: "alice"},
		"AKIATESTBBB": {AccessKey: "AKIATESTBBB", Owner: "bob"},
	})

	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		expr := r.URL.Query().Get("query")
		switch {
		case contains(expr, "strata_http_requests_total"):
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[
				{"metric":{"access_key":"AKIATESTAAA"},"value":[1700000000.0,"100"]},
				{"metric":{"access_key":"AKIATESTBBB"},"value":[1700000000.0,"42"]},
				{"metric":{"access_key":"AKIATESTCCC"},"value":[1700000000.0,"7"]}
			]}}`))
		case contains(expr, "strata_http_bytes_total"):
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[
				{"metric":{"access_key":"AKIATESTAAA"},"value":[1700000000.0,"1048576"]}
			]}}`))
		default:
			t.Errorf("unexpected query: %q", expr)
		}
	}))
	defer prom.Close()
	s.Prom = promclient.New(prom.URL)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/consumers/top?by=requests&limit=2", nil)
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got ConsumersTopResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.MetricsAvailable {
		t.Error("MetricsAvailable: want true")
	}
	if len(got.Consumers) != 2 {
		t.Fatalf("len=%d want 2", len(got.Consumers))
	}
	if got.Consumers[0].AccessKey != "AKIATESTAAA" || got.Consumers[0].User != "alice" {
		t.Errorf("first=%+v want alice/AAA", got.Consumers[0])
	}
	if got.Consumers[0].RequestCount24h != 100 || got.Consumers[0].Bytes24h != 1048576 {
		t.Errorf("first counts=%+v", got.Consumers[0])
	}
	if got.Consumers[1].AccessKey != "AKIATESTBBB" || got.Consumers[1].User != "bob" {
		t.Errorf("second=%+v want bob/BBB", got.Consumers[1])
	}
}

func TestConsumersTopByBytesSorts(t *testing.T) {
	s := newTestServer()
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		expr := r.URL.Query().Get("query")
		switch {
		case contains(expr, "strata_http_requests_total"):
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[
				{"metric":{"access_key":"k1"},"value":[1700000000.0,"1"]},
				{"metric":{"access_key":"k2"},"value":[1700000000.0,"2"]}
			]}}`))
		case contains(expr, "strata_http_bytes_total"):
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[
				{"metric":{"access_key":"k1"},"value":[1700000000.0,"999999"]},
				{"metric":{"access_key":"k2"},"value":[1700000000.0,"42"]}
			]}}`))
		}
	}))
	defer prom.Close()
	s.Prom = promclient.New(prom.URL)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/consumers/top?by=bytes", nil)
	s.routes().ServeHTTP(rr, req)
	var got ConsumersTopResponse
	_ = json.NewDecoder(rr.Body).Decode(&got)
	if len(got.Consumers) != 2 || got.Consumers[0].AccessKey != "k1" {
		t.Fatalf("got=%+v want k1 first", got.Consumers)
	}
}

func TestConsumersTopRejectsBadByParam(t *testing.T) {
	s := newTestServer()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/consumers/top?by=foo", nil)
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400", rr.Code)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
