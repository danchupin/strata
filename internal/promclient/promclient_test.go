package promclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestUnavailableWhenBaseURLEmpty(t *testing.T) {
	c := New("")
	if c.Available() {
		t.Fatal("Available() true on empty BaseURL")
	}
	if _, err := c.Query(context.Background(), "up"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("Query: got %v want ErrUnavailable", err)
	}
	if _, err := c.QueryRange(context.Background(), "up", time.Now(), time.Now(), time.Second); !errors.Is(err, ErrUnavailable) {
		t.Errorf("QueryRange: got %v want ErrUnavailable", err)
	}
}

func TestQueryParsesVector(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query" {
			t.Errorf("path: %q", r.URL.Path)
		}
		if r.URL.Query().Get("query") != "sum(rate(http[1m]))" {
			t.Errorf("query: %q", r.URL.Query().Get("query"))
		}
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[
			{"metric":{"bucket":"foo"},"value":[1700000000.0,"42"]},
			{"metric":{"bucket":"bar"},"value":[1700000000.0,"7.5"]}
		]}}`))
	}))
	defer srv.Close()

	c := New(srv.URL)
	got, err := c.Query(context.Background(), "sum(rate(http[1m]))")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len=%d want 2", len(got))
	}
	if got[0].Metric["bucket"] != "foo" || got[0].Value != 42 {
		t.Errorf("got[0]=%+v", got[0])
	}
	if got[1].Value != 7.5 {
		t.Errorf("got[1].Value=%v want 7.5", got[1].Value)
	}
}

func TestQueryUpstream500ReturnsUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("upstream down"))
	}))
	defer srv.Close()
	c := New(srv.URL)
	_, err := c.Query(context.Background(), "up")
	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("err=%v want ErrUnavailable", err)
	}
}

func TestQueryRangeParsesMatrix(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query_range" {
			t.Errorf("path: %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[
			{"metric":{"job":"strata"},"values":[[1700000000.0,"1"],[1700000060.0,"2.5"]]}
		]}}`))
	}))
	defer srv.Close()

	c := New(srv.URL)
	got, err := c.QueryRange(context.Background(), "rate(http[1m])",
		time.Unix(1_700_000_000, 0), time.Unix(1_700_000_060, 0), time.Minute)
	if err != nil {
		t.Fatalf("QueryRange: %v", err)
	}
	if len(got) != 1 || len(got[0].Points) != 2 {
		t.Fatalf("got=%+v", got)
	}
	if got[0].Points[1].Value != 2.5 {
		t.Errorf("Points[1].Value=%v want 2.5", got[0].Points[1].Value)
	}
}
