package adminapi

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/heartbeat"
	metamem "github.com/danchupin/strata/internal/meta/memory"
	"github.com/danchupin/strata/internal/s3api"
)

func newSettingsServer(t *testing.T, dir string) *Server {
	t.Helper()
	creds := auth.NewStaticStore(map[string]*auth.Credential{})
	s := New(Config{
		Meta:                 metamem.New(),
		Creds:                creds,
		Heartbeat:            heartbeat.NewMemoryStore(),
		Version:              "test-sha",
		ClusterName:          "test-cluster",
		Region:               "test-region",
		MetaBackend:          "cassandra",
		DataBackend:          "rados",
		JWTSecret:            []byte("0123456789abcdef0123456789abcdef"),
		JWTEphemeral:         true,
		JWTSecretFile:        filepath.Join(dir, "jwt-secret"),
		PrometheusURL:        "http://prom.local:9090",
		HeartbeatInterval:    10 * time.Second,
		ConsoleThemeDefault:  "system",
		AuditTTL:             720 * time.Hour,
		CassandraSettings: CassandraSettings{
			Hosts:       []string{"127.0.0.1:9042"},
			Keyspace:    "strata",
			LocalDC:     "datacenter1",
			Replication: "{'class': 'SimpleStrategy', 'replication_factor': '1'}",
			Username:    "admin",
		},
		RADOSSettings: RADOSSettings{
			ConfigFile: "/etc/ceph/ceph.conf",
			User:       "admin",
			Pool:       "strata.rgw.buckets.data",
			Classes:    "STANDARD:hot,GLACIER:cold",
		},
		TiKVSettings: TiKVSettings{Endpoints: []string{"pd1:2379"}},
	})
	s.Started = time.Unix(1_700_000_000, 0)
	return s
}

func TestSettingsGetShape(t *testing.T) {
	dir := t.TempDir()
	s := newSettingsServer(t, dir)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/settings", nil)
	s.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}
	var got SettingsResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Settings.ClusterName != "test-cluster" || got.Settings.Region != "test-region" {
		t.Errorf("identity: %+v", got.Settings)
	}
	if got.Settings.JWTSecret != "<ephemeral>" {
		t.Errorf("jwt secret label: %q want <ephemeral>", got.Settings.JWTSecret)
	}
	if !got.Settings.JWTEphemeral {
		t.Error("jwt ephemeral: false want true")
	}
	if got.Settings.JWTSecretFile != filepath.Join(dir, "jwt-secret") {
		t.Errorf("jwt file: %q", got.Settings.JWTSecretFile)
	}
	if got.Settings.PrometheusURL != "http://prom.local:9090" {
		t.Errorf("prometheus: %q", got.Settings.PrometheusURL)
	}
	if got.Settings.HeartbeatIntervalMS != 10_000 {
		t.Errorf("heartbeat ms: %d want 10000", got.Settings.HeartbeatIntervalMS)
	}
	if got.Settings.AuditRetentionMS != int64((720*time.Hour)/time.Millisecond) {
		t.Errorf("audit retention ms: %d", got.Settings.AuditRetentionMS)
	}
	if len(got.Backends.Cassandra.Hosts) != 1 || got.Backends.Cassandra.Hosts[0] != "127.0.0.1:9042" {
		t.Errorf("cassandra hosts: %+v", got.Backends.Cassandra.Hosts)
	}
	if got.Backends.RADOS.Pool != "strata.rgw.buckets.data" {
		t.Errorf("rados pool: %q", got.Backends.RADOS.Pool)
	}
	if len(got.Backends.TiKV.Endpoints) != 1 {
		t.Errorf("tikv endpoints: %+v", got.Backends.TiKV.Endpoints)
	}
}

func TestSettingsGetMaskedJWTPersisted(t *testing.T) {
	dir := t.TempDir()
	s := newSettingsServer(t, dir)
	s.JWTEphemeral = false
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/settings", nil)
	s.routes().ServeHTTP(rr, req)
	var got SettingsResponse
	_ = json.NewDecoder(rr.Body).Decode(&got)
	if got.Settings.JWTSecret != "<set>" {
		t.Errorf("label: %q want <set>", got.Settings.JWTSecret)
	}
	if got.Settings.JWTEphemeral {
		t.Error("ephemeral: true want false")
	}
}

func TestSettingsRotateJWT(t *testing.T) {
	dir := t.TempDir()
	s := newSettingsServer(t, dir)
	prev := append([]byte(nil), s.JWTSecret...)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/v1/settings/jwt/rotate", nil)
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}
	var resp RotateJWTResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.File != filepath.Join(dir, "jwt-secret") {
		t.Errorf("file: %q", resp.File)
	}

	got := s.jwtSecret()
	if string(got) == string(prev) {
		t.Fatal("secret did not change")
	}
	if len(got) != 32 {
		t.Errorf("secret len: %d want 32", len(got))
	}
	if s.jwtEphemeral() {
		t.Error("ephemeral: true want false after rotation")
	}

	raw, err := os.ReadFile(filepath.Join(dir, "jwt-secret"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	decoded, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("hex decode: %v", err)
	}
	if string(decoded) != string(got) {
		t.Errorf("disk content does not match in-memory secret")
	}
	info, err := os.Stat(filepath.Join(dir, "jwt-secret"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("perms: %o want 0600", mode)
	}
}

func TestSettingsRotateInvalidatesPriorCookie(t *testing.T) {
	dir := t.TempDir()
	creds := auth.NewStaticStore(map[string]*auth.Credential{
		"AKIATEST": {AccessKey: "AKIATEST", Secret: "secret", Owner: "alice"},
	})
	s := New(Config{
		Meta:          metamem.New(),
		Creds:         creds,
		Heartbeat:     heartbeat.NewMemoryStore(),
		Version:       "test-sha",
		ClusterName:   "test-cluster",
		Region:        "test-region",
		MetaBackend:   "memory",
		DataBackend:   "memory",
		JWTSecret:     []byte("0123456789abcdef0123456789abcdef"),
		JWTEphemeral:  true,
		JWTSecretFile: filepath.Join(dir, "jwt-secret"),
	})

	tok, _, err := signSession(s.jwtSecret(), "AKIATEST", time.Now())
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/v1/settings/jwt/rotate", nil)
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("rotate status: %d body=%s", rr.Code, rr.Body.String())
	}

	rr2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/admin/v1/cluster/status", nil)
	req2.AddCookie(&http.Cookie{Name: sessionCookieName, Value: tok})
	s.Handler().ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusUnauthorized {
		t.Errorf("post-rotate status: %d want 401", rr2.Code)
	}
}

func TestSettingsRotateAuditRow(t *testing.T) {
	dir := t.TempDir()
	s := newSettingsServer(t, dir)

	mw := s3api.NewAuditMiddleware(s.Meta, time.Hour, s.routes())
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/v1/settings/jwt/rotate", nil)
	req = req.WithContext(auth.WithAuth(req.Context(), &auth.AuthInfo{AccessKey: "AKIATEST", Owner: "alice"}))
	mw.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}
	rows, err := s.Meta.ListAudit(context.Background(), uuid.Nil, 100)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("audit rows: %d want 1", len(rows))
	}
	if rows[0].Action != "admin:RotateJWTSecret" {
		t.Errorf("action: %q", rows[0].Action)
	}
	if rows[0].Resource != "settings:jwt" {
		t.Errorf("resource: %q", rows[0].Resource)
	}
	if rows[0].Principal != "alice" {
		t.Errorf("principal: %q", rows[0].Principal)
	}
}

func TestSettingsDataBackendMemory(t *testing.T) {
	dir := t.TempDir()
	s := newSettingsServer(t, dir)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/settings/data-backend", nil)
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d", rr.Code)
	}
	var got S3BackendSettings
	_ = json.NewDecoder(rr.Body).Decode(&got)
	if got.Kind != "" {
		t.Errorf("kind: %q want empty (data_backend != s3)", got.Kind)
	}
}

// TestSettingsDataBackendS3MultiCluster pins the US-004 wire shape:
// /admin/v1/settings/data-backend echoes the STRATA_S3_CLUSTERS /
// STRATA_S3_CLASSES JSON blobs verbatim so the admin console can render
// the multi-cluster routing table. The blobs reference credentials by
// envelope (chain / env: / file:) so no plaintext keys leak.
func TestSettingsDataBackendS3MultiCluster(t *testing.T) {
	dir := t.TempDir()
	s := newSettingsServer(t, dir)
	s.DataBackend = "s3"
	clusters := `[{"id":"primary","endpoint":"https://s3.example.com","region":"us-east-1","force_path_style":true,"credentials":{"type":"chain"}}]`
	classes := `{"STANDARD":{"cluster":"primary","bucket":"hot"}}`
	s.S3Backend = S3BackendSettings{
		Kind:     "s3",
		Clusters: clusters,
		Classes:  classes,
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/settings/data-backend", nil)
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d", rr.Code)
	}
	body := rr.Body.String()
	if strings.Contains(body, "access_key") || strings.Contains(body, "secret_key") {
		t.Errorf("response leaks key fields: %s", body)
	}
	var got S3BackendSettings
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Kind != "s3" {
		t.Errorf("kind: %q want s3", got.Kind)
	}
	if got.Clusters != clusters {
		t.Errorf("clusters mismatch:\n got=%s\nwant=%s", got.Clusters, clusters)
	}
	if got.Classes != classes {
		t.Errorf("classes mismatch:\n got=%s\nwant=%s", got.Classes, classes)
	}
}

