package serverapp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/danchupin/strata/internal/adminapi"
	"github.com/danchupin/strata/internal/config"
	datamem "github.com/danchupin/strata/internal/data/memory"
	metamem "github.com/danchupin/strata/internal/meta/memory"
)

// fakeMeta wraps memory.Store and reports a Probe error so the cassandra
// branch in buildHealthHandler is exercised end-to-end without a live
// gocql session.
type fakeMeta struct {
	*metamem.Store
	probeErr error
}

func (f *fakeMeta) Probe(_ context.Context) error { return f.probeErr }

type fakeData struct {
	*datamem.Backend
	probeErr error
}

func (f *fakeData) Probe(_ context.Context, _ string) error { return f.probeErr }

func TestBuildHealthHandlerMemoryBackendsHaveNoProbes(t *testing.T) {
	h := buildHealthHandler(metamem.New(), datamem.New())
	if got := len(h.Probes); got != 0 {
		t.Fatalf("memory backends should register no probes, got %d", got)
	}

	w := httptest.NewRecorder()
	h.Healthz(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if w.Code != http.StatusOK || w.Body.String() != "ok" {
		t.Fatalf("/healthz status=%d body=%q want 200 ok", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	h.Readyz(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("/readyz status=%d body=%q want 200 (no probes => ready)", w.Code, w.Body.String())
	}
}

func TestBuildHealthHandlerProbersAllOK(t *testing.T) {
	h := buildHealthHandler(
		&fakeMeta{Store: metamem.New()},
		&fakeData{Backend: datamem.New()},
	)
	if got := len(h.Probes); got != 2 {
		t.Fatalf("expected 2 probes (cassandra+rados), got %d", got)
	}
	w := httptest.NewRecorder()
	h.Readyz(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("/readyz status=%d body=%q want 200", w.Code, w.Body.String())
	}
}

func TestBuildHealthHandlerCassandraDown(t *testing.T) {
	h := buildHealthHandler(
		&fakeMeta{Store: metamem.New(), probeErr: errors.New("connection refused")},
		&fakeData{Backend: datamem.New()},
	)
	w := httptest.NewRecorder()
	h.Readyz(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz status=%d want 503", w.Code)
	}

	w = httptest.NewRecorder()
	h.Healthz(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("/healthz status=%d want 200 (must not consult probes)", w.Code)
	}
}

func TestBuildHealthHandlerRadosDown(t *testing.T) {
	h := buildHealthHandler(
		&fakeMeta{Store: metamem.New()},
		&fakeData{Backend: datamem.New(), probeErr: errors.New("ENOENT pool")},
	)
	w := httptest.NewRecorder()
	h.Readyz(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz status=%d want 503", w.Code)
	}
}

func TestParseTiKVEndpoints(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", []string{}},
		{"pd:2379", []string{"pd:2379"}},
		{"pd-1:2379,pd-2:2379, pd-3:2379 ", []string{"pd-1:2379", "pd-2:2379", "pd-3:2379"}},
		{",,pd:2379,,", []string{"pd:2379"}},
	}
	for _, tc := range cases {
		got := parseTiKVEndpoints(tc.in)
		if len(got) == 0 && len(tc.want) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("parseTiKVEndpoints(%q)=%v want %v", tc.in, got, tc.want)
		}
	}
}

func TestBuildMetaStoreTiKVEmptyEndpointsRejected(t *testing.T) {
	cfg := &config.Config{MetaBackend: "tikv"}
	store, err := buildMetaStore(cfg, nil, nil)
	if err == nil {
		_ = store.Close()
		t.Fatal("buildMetaStore(tikv) with empty endpoints should fail")
	}
}

func TestBuildLockerTiKVNilWhenStoreMismatched(t *testing.T) {
	cfg := &config.Config{MetaBackend: "tikv"}
	if got := buildLocker(cfg, metamem.New()); got != nil {
		t.Fatalf("buildLocker(tikv) with non-tikv store should return nil, got %T", got)
	}
}

// TestS3BackendSettingsNonS3DataBackendIsZero asserts the helper returns the
// empty struct (Kind="") whenever the gateway is not running on the s3 data
// backend. The Settings page Backends tab keys the "S3 Backend" subsection
// off Kind=="s3" — non-s3 deployments must not surface a stray card.
func TestS3BackendSettingsNonS3DataBackendIsZero(t *testing.T) {
	for _, kind := range []string{"", "memory", "rados"} {
		cfg := &config.Config{DataBackend: kind}
		got := s3BackendSettings(cfg)
		if got != (adminapi.S3BackendSettings{}) {
			t.Fatalf("data_backend=%q: want zero S3BackendSettings, got %+v", kind, got)
		}
	}
}

// TestS3BackendSettingsMasksKeys is the US-021 wire-shape lockdown: when
// data_backend=s3, every config field surfaces verbatim EXCEPT AccessKey /
// SecretKey, which collapse into AccessKeySet / SecretKeySet booleans. The
// raw secrets must never reach the admin layer.
func TestS3BackendSettingsMasksKeys(t *testing.T) {
	cfg := &config.Config{
		DataBackend: "s3",
		S3Backend: config.S3BackendConfig{
			Endpoint:          "https://minio:9000",
			Region:            "us-east-1",
			Bucket:            "primary",
			AccessKey:         "AKIAEXAMPLE",
			SecretKey:         "supersecret",
			ForcePathStyle:    true,
			PartSize:          16 * 1024 * 1024,
			UploadConcurrency: 8,
			MaxRetries:        5,
			OpTimeoutSecs:     30,
			SSEMode:           "passthrough",
			SSEKMSKeyID:       "arn:aws:kms:us-east-1:111122223333:key/abc",
		},
	}
	got := s3BackendSettings(cfg)
	want := adminapi.S3BackendSettings{
		Kind:              "s3",
		Endpoint:          "https://minio:9000",
		Region:            "us-east-1",
		Bucket:            "primary",
		ForcePathStyle:    true,
		PartSize:          16 * 1024 * 1024,
		UploadConcurrency: 8,
		MaxRetries:        5,
		OpTimeoutSecs:     30,
		SSEMode:           "passthrough",
		SSEKMSKeyID:       "arn:aws:kms:us-east-1:111122223333:key/abc",
		AccessKeySet:      true,
		SecretKeySet:      true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("s3BackendSettings:\n got=%+v\nwant=%+v", got, want)
	}
}

// TestS3BackendSettingsKeyPresenceFlags asserts AccessKeySet / SecretKeySet
// flip independently with key presence — covers the SDK-default-chain shape
// (both empty, both flags false) plus partial-set rows for completeness.
func TestS3BackendSettingsKeyPresenceFlags(t *testing.T) {
	cases := []struct {
		ak, sk           string
		wantAK, wantSK   bool
	}{
		{"", "", false, false},
		{"AKIA", "", true, false},
		{"", "secret", false, true},
		{"AKIA", "secret", true, true},
	}
	for _, tc := range cases {
		cfg := &config.Config{
			DataBackend: "s3",
			S3Backend: config.S3BackendConfig{
				Bucket: "b", Region: "r",
				AccessKey: tc.ak, SecretKey: tc.sk,
			},
		}
		got := s3BackendSettings(cfg)
		if got.AccessKeySet != tc.wantAK || got.SecretKeySet != tc.wantSK {
			t.Errorf("ak=%q sk=%q: AccessKeySet=%v SecretKeySet=%v want %v / %v",
				tc.ak, tc.sk, got.AccessKeySet, got.SecretKeySet, tc.wantAK, tc.wantSK)
		}
	}
}

func TestHealthCanaryOIDDefault(t *testing.T) {
	t.Setenv("STRATA_RADOS_HEALTH_OID", "")
	if got := healthCanaryOID(); got != "strata-readyz-canary" {
		t.Fatalf("default OID=%q", got)
	}
	t.Setenv("STRATA_RADOS_HEALTH_OID", "custom-oid")
	if got := healthCanaryOID(); got != "custom-oid" {
		t.Fatalf("override OID=%q", got)
	}
}
