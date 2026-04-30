package config

import (
	"os"
	"strings"
	"testing"
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
