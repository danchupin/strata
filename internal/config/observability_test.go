package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLoadObservabilityFromTOMLOnly proves the otel.* + logging.* +
// audit_log.* TOML sections drive every per-knob tunable end-to-end with
// zero STRATA_* env vars set. Parity AC for US-004.
func TestLoadObservabilityFromTOMLOnly(t *testing.T) {
	clearEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")

	body := `
[otel]
endpoint      = "http://collector:4318"
sample_ratio  = 0.25
ringbuf       = false
ringbuf_bytes = 8388608

[logging]
level  = "DEBUG"
format = "text"

[audit_log]
retention = "168h"
`
	dir := t.TempDir()
	path := filepath.Join(dir, "strata.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write toml: %v", err)
	}
	t.Setenv("STRATA_CONFIG_FILE", path)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.OTel.Endpoint != "http://collector:4318" {
		t.Errorf("otel.endpoint = %q", cfg.OTel.Endpoint)
	}
	if cfg.OTel.SampleRatio != 0.25 {
		t.Errorf("otel.sample_ratio = %v", cfg.OTel.SampleRatio)
	}
	if cfg.OTel.Ringbuf {
		t.Errorf("otel.ringbuf = %v want false", cfg.OTel.Ringbuf)
	}
	if cfg.OTel.RingbufBytes != 8388608 {
		t.Errorf("otel.ringbuf_bytes = %d", cfg.OTel.RingbufBytes)
	}
	if cfg.Logging.Level != "DEBUG" {
		t.Errorf("logging.level = %q", cfg.Logging.Level)
	}
	if cfg.Logging.Format != "text" {
		t.Errorf("logging.format = %q", cfg.Logging.Format)
	}
	if cfg.AuditLog.Retention != 168*time.Hour {
		t.Errorf("audit_log.retention = %v", cfg.AuditLog.Retention)
	}
}

// TestEnvOverridesTOMLForObservability pins env > TOML precedence for
// the otel + logging + audit_log knobs. Also exercises the legacy
// STRATA_AUDIT_RETENTION "<N>d" suffix parse path on the env side.
func TestEnvOverridesTOMLForObservability(t *testing.T) {
	clearEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")

	body := `
[otel]
endpoint      = "http://collector.toml:4318"
sample_ratio  = 0.25
ringbuf       = false
ringbuf_bytes = 8388608

[logging]
level  = "INFO"
format = "json"

[audit_log]
retention = "168h"
`
	dir := t.TempDir()
	path := filepath.Join(dir, "strata.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write toml: %v", err)
	}
	t.Setenv("STRATA_CONFIG_FILE", path)
	t.Setenv("STRATA_OTEL_EXPORTER_ENDPOINT", "http://collector.env:4318")
	t.Setenv("STRATA_OTEL_SAMPLE_RATIO", "0.5")
	t.Setenv("STRATA_OTEL_RINGBUF", "true")
	t.Setenv("STRATA_OTEL_RINGBUF_BYTES", "2097152")
	t.Setenv("STRATA_LOG_LEVEL", "WARN")
	t.Setenv("STRATA_LOG_FORMAT", "json")
	t.Setenv("STRATA_AUDIT_RETENTION", "7d")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.OTel.Endpoint != "http://collector.env:4318" {
		t.Errorf("otel.endpoint env override: %q", cfg.OTel.Endpoint)
	}
	if cfg.OTel.SampleRatio != 0.5 {
		t.Errorf("otel.sample_ratio env override: %v", cfg.OTel.SampleRatio)
	}
	if !cfg.OTel.Ringbuf {
		t.Errorf("otel.ringbuf env override: %v want true", cfg.OTel.Ringbuf)
	}
	if cfg.OTel.RingbufBytes != 2097152 {
		t.Errorf("otel.ringbuf_bytes env override: %d", cfg.OTel.RingbufBytes)
	}
	if cfg.Logging.Level != "WARN" {
		t.Errorf("logging.level env override: %q", cfg.Logging.Level)
	}
	if cfg.AuditLog.Retention != 7*24*time.Hour {
		t.Errorf("audit_log.retention env override: %v want 168h", cfg.AuditLog.Retention)
	}
}

// TestOTelEndpointFallsBackToOTELExporter pins the
// OTEL_EXPORTER_OTLP_ENDPOINT bootstrap-env fallback. STRATA_OTEL_EXPORTER_ENDPOINT
// wins when set; OTEL_EXPORTER_OTLP_ENDPOINT fills the empty default.
func TestOTelEndpointFallsBackToOTELExporter(t *testing.T) {
	clearEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://collector.fallback:4318")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.OTel.Endpoint != "http://collector.fallback:4318" {
		t.Errorf("otel.endpoint OTEL_EXPORTER_OTLP_ENDPOINT fallback: %q", cfg.OTel.Endpoint)
	}

	t.Setenv("STRATA_OTEL_EXPORTER_ENDPOINT", "http://collector.strata:4318")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.OTel.Endpoint != "http://collector.strata:4318" {
		t.Errorf("otel.endpoint STRATA_OTEL_EXPORTER_ENDPOINT precedence: %q", cfg.OTel.Endpoint)
	}
}

// TestClampObservabilityOnLoad enforces range clamps for both env and
// TOML sources, matching the legacy env-only clamps in internal/otel.
func TestClampObservabilityOnLoad(t *testing.T) {
	clearEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")

	body := `
[otel]
sample_ratio  = 5.0
ringbuf_bytes = 500

[audit_log]
retention = "1000000h"
`
	dir := t.TempDir()
	path := filepath.Join(dir, "strata.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write toml: %v", err)
	}
	t.Setenv("STRATA_CONFIG_FILE", path)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.OTel.SampleRatio != 1.0 {
		t.Errorf("otel.sample_ratio clamp: %v want 1.0", cfg.OTel.SampleRatio)
	}
	if cfg.OTel.RingbufBytes != 1<<20 {
		t.Errorf("otel.ringbuf_bytes clamp: %d want %d", cfg.OTel.RingbufBytes, 1<<20)
	}
	if cfg.AuditLog.Retention != 10*365*24*time.Hour {
		t.Errorf("audit_log.retention clamp: %v want 10y", cfg.AuditLog.Retention)
	}
}

// TestNegativeOTelSampleRatioClamps pins the lower bound separately —
// the upper-bound test exercises the same clamp branch.
func TestNegativeOTelSampleRatioClamps(t *testing.T) {
	clearEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("STRATA_OTEL_SAMPLE_RATIO", "-0.5")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.OTel.SampleRatio != 0 {
		t.Errorf("otel.sample_ratio negative clamp: %v want 0", cfg.OTel.SampleRatio)
	}
}

// TestDefaultObservabilityKnobs pins the default values that the
// drift-lint in US-006 will hash against.
func TestDefaultObservabilityKnobs(t *testing.T) {
	clearEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.OTel.Endpoint != "" {
		t.Errorf("otel.endpoint default: %q want empty", cfg.OTel.Endpoint)
	}
	if cfg.OTel.SampleRatio != 0.01 {
		t.Errorf("otel.sample_ratio default: %v want 0.01", cfg.OTel.SampleRatio)
	}
	if !cfg.OTel.Ringbuf {
		t.Errorf("otel.ringbuf default: %v want true", cfg.OTel.Ringbuf)
	}
	if cfg.OTel.RingbufBytes != 4<<20 {
		t.Errorf("otel.ringbuf_bytes default: %d want %d", cfg.OTel.RingbufBytes, 4<<20)
	}
	if cfg.Logging.Level != "INFO" {
		t.Errorf("logging.level default: %q want INFO", cfg.Logging.Level)
	}
	if cfg.Logging.Format != "json" {
		t.Errorf("logging.format default: %q want json", cfg.Logging.Format)
	}
	if cfg.AuditLog.Retention != 30*24*time.Hour {
		t.Errorf("audit_log.retention default: %v want 720h", cfg.AuditLog.Retention)
	}
}
