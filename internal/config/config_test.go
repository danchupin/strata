package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadDefaultsMemoryBackend(t *testing.T) {
	clearEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MetaBackend != "memory" {
		t.Fatalf("default meta_backend=%q want memory", cfg.MetaBackend)
	}
	if cfg.TiKV.Endpoints != "" {
		t.Fatalf("default tikv.pd_endpoints=%q want empty", cfg.TiKV.Endpoints)
	}
}

func TestLoadTiKVBackendRequiresEndpoints(t *testing.T) {
	clearEnv(t)
	t.Setenv("STRATA_META_BACKEND", "tikv")
	_, err := Load()
	if err == nil {
		t.Fatal("Load with STRATA_META_BACKEND=tikv and no endpoints should fail")
	}
	if !strings.Contains(err.Error(), "STRATA_TIKV_PD_ENDPOINTS") {
		t.Fatalf("error %q must mention STRATA_TIKV_PD_ENDPOINTS", err.Error())
	}
}

func TestLoadTiKVBackendAcceptsEndpoints(t *testing.T) {
	clearEnv(t)
	t.Setenv("STRATA_META_BACKEND", "tikv")
	t.Setenv("STRATA_TIKV_PD_ENDPOINTS", "pd-1:2379,pd-2:2379")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MetaBackend != "tikv" {
		t.Fatalf("meta_backend=%q want tikv", cfg.MetaBackend)
	}
	if cfg.TiKV.Endpoints != "pd-1:2379,pd-2:2379" {
		t.Fatalf("tikv.pd_endpoints=%q", cfg.TiKV.Endpoints)
	}
}

func TestLoadUnknownMetaBackendRejected(t *testing.T) {
	clearEnv(t)
	t.Setenv("STRATA_META_BACKEND", "etcd")
	_, err := Load()
	if err == nil {
		t.Fatal("unknown meta_backend should fail validation")
	}
	if !strings.Contains(err.Error(), "tikv") {
		t.Fatalf("error %q should advertise tikv as a valid option", err.Error())
	}
}

// clearEnv removes every STRATA_-prefixed env var from the parent shell
// so each test case reads a clean baseline. t.Cleanup restores the
// originals when the test exits.
func clearEnv(t *testing.T) {
	t.Helper()
	saved := map[string]string{}
	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if strings.HasPrefix(k, "STRATA_") {
			saved[k] = v
			os.Unsetenv(k)
		}
	}
	t.Cleanup(func() {
		for k, v := range saved {
			os.Setenv(k, v)
		}
	})
}

// TestLoadRejectsS3WithoutClusters pins the boot-time fail-fast for
// US-004: setting STRATA_DATA_BACKEND=s3 without STRATA_S3_CLUSTERS must
// fail Load() with a clear message — the operator finds out at startup,
// not at first request.
func TestLoadRejectsS3WithoutClusters(t *testing.T) {
	clearEnv(t)
	t.Setenv("STRATA_DATA_BACKEND", "s3")
	t.Setenv("STRATA_S3_CLASSES", `{"STANDARD":{"cluster":"primary","bucket":"hot"}}`)

	_, err := Load()
	if err == nil {
		t.Fatal("want error from Load with empty STRATA_S3_CLUSTERS, got nil")
	}
	if !strings.Contains(err.Error(), "STRATA_S3_CLUSTERS") {
		t.Fatalf("want error mentioning STRATA_S3_CLUSTERS, got %v", err)
	}
}

// TestLoadRejectsS3WithoutClasses pins the symmetric check: classes env
// missing → boot-time error.
func TestLoadRejectsS3WithoutClasses(t *testing.T) {
	clearEnv(t)
	t.Setenv("STRATA_DATA_BACKEND", "s3")
	t.Setenv("STRATA_S3_CLUSTERS", `[{"id":"primary","endpoint":"https://s3","region":"us-east-1","credentials":{"type":"chain"}}]`)

	_, err := Load()
	if err == nil {
		t.Fatal("want error from Load with empty STRATA_S3_CLASSES, got nil")
	}
	if !strings.Contains(err.Error(), "STRATA_S3_CLASSES") {
		t.Fatalf("want error mentioning STRATA_S3_CLASSES, got %v", err)
	}
}

// TestLoadAcceptsS3DataBackend pins env-var wiring for the new shape:
// setting STRATA_DATA_BACKEND=s3 + the two JSON envs yields a Config that
// validates and carries the raw JSON blobs verbatim for downstream
// parsing.
func TestLoadAcceptsS3DataBackend(t *testing.T) {
	clearEnv(t)
	clustersJSON := `[{"id":"primary","endpoint":"http://minio:9000","region":"us-east-1","force_path_style":true,"credentials":{"type":"env","ref":"AK_VAR:SK_VAR"}}]`
	classesJSON := `{"STANDARD":{"cluster":"primary","bucket":"hot-tier"}}`
	t.Setenv("STRATA_DATA_BACKEND", "s3")
	t.Setenv("STRATA_S3_CLUSTERS", clustersJSON)
	t.Setenv("STRATA_S3_CLASSES", classesJSON)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DataBackend != "s3" {
		t.Fatalf("data_backend: want s3, got %q", cfg.DataBackend)
	}
	if cfg.S3.Clusters != clustersJSON {
		t.Fatalf("s3.clusters: want %q, got %q", clustersJSON, cfg.S3.Clusters)
	}
	if cfg.S3.Classes != classesJSON {
		t.Fatalf("s3.classes: want %q, got %q", classesJSON, cfg.S3.Classes)
	}
}

// TestLoadEmptyEnvDoesNotOverrideTOMLValue pins the koanf empty-env regression
// surfaced by the prior cycle (US-006 of ralph/s3-compat-95): an empty-string
// STRATA_* env var must NOT clobber a TOML-loaded value (or the in-memory
// default). The fix lives in Load's env.ProviderWithValue callback, which now
// returns ("", nil) when the env value is empty so koanf treats the slot as
// unset. Without that callback, koanf's env provider stomped TOML/defaults
// with the empty string and the typed Unmarshal silently zeroed the field.
//
// The TOML shape exercises both code paths (file-load + env-merge) so a future
// regression in either provider's empty-handling fails this test.
func TestLoadEmptyEnvDoesNotOverrideTOMLValue(t *testing.T) {
	clearEnv(t)

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "strata.toml")
	if err := os.WriteFile(cfgPath, []byte("[gc]\ninterval = \"30s\"\n"), 0o600); err != nil {
		t.Fatalf("write toml: %v", err)
	}
	t.Setenv("STRATA_CONFIG_FILE", cfgPath)
	t.Setenv("STRATA_GC_INTERVAL", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.GC.Interval != 30*time.Second {
		t.Fatalf("gc.interval after empty STRATA_GC_INTERVAL: got %s want 30s "+
			"(empty env regressed: re-clobbering TOML/default)", cfg.GC.Interval)
	}
}
