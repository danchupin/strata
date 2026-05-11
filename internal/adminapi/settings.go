package adminapi

import (
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/s3api"
)

// Settings is the JSON shape served by GET /admin/v1/settings (US-019). All
// fields read-only. Secrets are never echoed: the JWT key surfaces as
// JWTSecret = "<set>" or "<ephemeral>" via JWTSecretSource so the operator
// can tell whether the cluster is running on a generated-at-startup key
// (which invalidates every session on restart) or a persisted one.
type Settings struct {
	ClusterName         string `json:"cluster_name"`
	Region              string `json:"region"`
	Version             string `json:"version"`
	PrometheusURL       string `json:"prometheus_url"`
	HeartbeatIntervalMS int64  `json:"heartbeat_interval_ms"`
	JWTSecret           string `json:"jwt_secret"`
	JWTEphemeral        bool   `json:"jwt_ephemeral"`
	JWTSecretFile       string `json:"jwt_secret_file"`
	ConsoleThemeDefault string `json:"console_theme_default"`
	AuditRetentionMS    int64  `json:"audit_retention_ms"`
	MetaBackend         string `json:"meta_backend"`
	DataBackend         string `json:"data_backend"`
}

// CassandraSettings carries Cassandra (or Scylla) connection parameters
// surfaced on the Backends tab. Empty when meta_backend != "cassandra".
type CassandraSettings struct {
	Hosts       []string `json:"hosts"`
	Keyspace    string   `json:"keyspace"`
	LocalDC     string   `json:"local_dc"`
	Replication string   `json:"replication"`
	Username    string   `json:"username,omitempty"`
}

// RADOSSettings carries the RADOS data backend connection parameters.
// Empty when data_backend != "rados".
type RADOSSettings struct {
	ConfigFile string `json:"config_file"`
	User       string `json:"user"`
	Pool       string `json:"pool"`
	Namespace  string `json:"namespace,omitempty"`
	Classes    string `json:"classes,omitempty"`
	Clusters   string `json:"clusters,omitempty"`
}

// TiKVSettings carries the TiKV PD endpoint list. Empty when
// meta_backend != "tikv".
type TiKVSettings struct {
	Endpoints []string `json:"endpoints"`
}

// SettingsBackends bundles the per-kind backend configs so the UI can render
// the Backends tab from a single response.
type SettingsBackends struct {
	Cassandra CassandraSettings `json:"cassandra"`
	RADOS     RADOSSettings     `json:"rados"`
	TiKV      TiKVSettings      `json:"tikv"`
}

// SettingsResponse is the wire shape returned by GET /admin/v1/settings.
// Splits the cluster/console knobs from the per-backend connection blobs so
// the React tab structure (Cluster | Console | Backends) maps 1:1 to the
// JSON without forcing the UI to merge two queries.
type SettingsResponse struct {
	Settings Settings         `json:"settings"`
	Backends SettingsBackends `json:"backends"`
}

// S3BackendSettings is the JSON shape served by GET /admin/v1/settings/
// data-backend (US-021 / US-004) for the s3-over-s3 data backend.
// Clusters and Classes mirror STRATA_S3_CLUSTERS / STRATA_S3_CLASSES
// verbatim — the operator console renders the raw JSON. Credentials
// inside the cluster spec are referenced (chain / env: / file:) but
// never include plaintext access keys, so no masking is needed at this
// layer. Empty kind ("") when the gateway runs on a non-s3 data backend.
type S3BackendSettings struct {
	Kind     string `json:"kind"`
	Clusters string `json:"clusters"`
	Classes  string `json:"classes"`
}

// DefaultJWTSecretFile is the on-disk path used when STRATA_JWT_SECRET_FILE
// is unset. Matches the AC default; production deployments are expected to
// override (read-only ConfigMap mount, etc.).
const DefaultJWTSecretFile = "/etc/strata/jwt-secret"

// handleGetSettings serves GET /admin/v1/settings.
func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	jwtLabel := "<set>"
	if s.jwtEphemeral() {
		jwtLabel = "<ephemeral>"
	}
	if len(s.jwtSecret()) == 0 {
		jwtLabel = "<unset>"
	}
	resp := SettingsResponse{
		Settings: Settings{
			ClusterName:         s.ClusterName,
			Region:              s.Region,
			Version:             s.Version,
			PrometheusURL:       s.PrometheusURL,
			HeartbeatIntervalMS: s.HeartbeatInterval.Milliseconds(),
			JWTSecret:           jwtLabel,
			JWTEphemeral:        s.jwtEphemeral(),
			JWTSecretFile:       s.JWTSecretFile,
			ConsoleThemeDefault: s.ConsoleThemeDefault,
			AuditRetentionMS:    s.AuditTTL.Milliseconds(),
			MetaBackend:         s.MetaBackend,
			DataBackend:         s.DataBackend,
		},
		Backends: SettingsBackends{
			Cassandra: s.CassandraSettings,
			RADOS:     s.RADOSSettings,
			TiKV:      s.TiKVSettings,
		},
	}
	if resp.Backends.Cassandra.Hosts == nil {
		resp.Backends.Cassandra.Hosts = []string{}
	}
	if resp.Backends.TiKV.Endpoints == nil {
		resp.Backends.TiKV.Endpoints = []string{}
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleGetSettingsDataBackend serves GET /admin/v1/settings/data-backend.
// Returns the s3-backend config (US-021) with access keys masked to booleans.
// Always returns 200 — kind="" when the gateway runs on memory/rados.
func (s *Server) handleGetSettingsDataBackend(w http.ResponseWriter, r *http.Request) {
	out := s.S3Backend
	if s.DataBackend != "s3" {
		out = S3BackendSettings{Kind: ""}
	} else if out.Kind == "" {
		out.Kind = "s3"
	}
	writeJSON(w, http.StatusOK, out)
}

// RotateJWTResponse is the body returned by POST /admin/v1/settings/jwt/rotate.
type RotateJWTResponse struct {
	RotatedAt int64  `json:"rotated_at"`
	File      string `json:"file"`
}

// handleRotateJWTSecret serves POST /admin/v1/settings/jwt/rotate. Mints a
// fresh 32-byte HS256 key, writes it (atomic rename) to JWTSecretFile, then
// flips the in-memory key. Every cookie issued under the previous key now
// fails verifySession on the next request — the UI takes the 401 and
// reroutes to /login. Audit row stamped admin:RotateJWTSecret.
func (s *Server) handleRotateJWTSecret(w http.ResponseWriter, r *http.Request) {
	target := s.JWTSecretFile
	if target == "" {
		target = DefaultJWTSecretFile
	}

	secret, err := GenerateSecret()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "InternalError", "generate secret: "+err.Error())
		return
	}

	if err := writeJWTSecretFile(target, secret); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "SecretFileWrite", err.Error())
		return
	}

	s.SetJWTSecret(secret)

	owner := ""
	if info := auth.FromContext(r.Context()); info != nil {
		owner = info.Owner
	}
	s3api.SetAuditOverride(r.Context(), "admin:RotateJWTSecret", "settings:jwt", "-", owner)

	writeJSON(w, http.StatusOK, RotateJWTResponse{
		RotatedAt: time.Now().Unix(),
		File:      target,
	})
}

// writeJWTSecretFile writes the hex-encoded secret to path atomically:
// emit to a sibling temp file under the same directory, fsync, then rename.
// Permission 0600 — the file holds a session-signing key.
func writeJWTSecretFile(path string, secret []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".jwt-secret.*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod: %w", err)
	}
	encoded := []byte(hex.EncodeToString(secret))
	if _, err := tmp.Write(encoded); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("fsync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	cleanup = false
	return nil
}
