package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLoadWorkersFromTOMLOnly proves the workers.* TOML section drives
// every per-worker tunable end-to-end with zero STRATA_* env vars set.
// This is the parity AC for US-002: an operator can fully configure each
// worker through deploy/strata.toml.example without touching env.
func TestLoadWorkersFromTOMLOnly(t *testing.T) {
	clearEnv(t)

	body := `
[workers]
enabled = "gc,lifecycle,rebalance"

[workers.gc]
interval = "11s"
grace = "12m"
batch_size = 250
concurrency = 4
shards = 8
dual_write = true
metrics_listen = ":9300"

[workers.lifecycle]
interval = "13s"
unit = "hour"
concurrency = 3
metrics_listen = ":9301"

[workers.rebalance]
interval = "7m"
rate_mb_s = 250
inflight = 16
shards = 4

[workers.usage_rollup]
at = "02:30"
interval = "12h"
samples_per_day = 48

[workers.manifest_rewriter]
interval = "30m"
batch_limit = 1000
dry_run = true

[workers.audit_export]
bucket = "audit-archive"
prefix = "exports/"
after = "168h"
interval = "1h"

[workers.quota_reconcile]
interval = "30m"

[workers.notify]
targets = "primary=https://example.com/hook|secret"
interval = "9s"
max_retries = 3
backoff_base = "750ms"
poll_limit = 42

[workers.replicator]
interval = "9s"
max_retries = 3
backoff_base = "750ms"
poll_limit = 42
http_timeout = "11s"
peer_scheme = "http"

[workers.access_log]
interval = "2m"
max_flush_bytes = 12345
poll_limit = 99

[workers.inventory]
interval = "3m"
region = "eu-west-1"
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

	if cfg.Workers.Enabled != "gc,lifecycle,rebalance" {
		t.Errorf("workers.enabled = %q", cfg.Workers.Enabled)
	}

	gc := cfg.Workers.GC
	if gc.Interval != 11*time.Second {
		t.Errorf("gc.interval = %v", gc.Interval)
	}
	if gc.Grace != 12*time.Minute {
		t.Errorf("gc.grace = %v", gc.Grace)
	}
	if gc.BatchSize != 250 {
		t.Errorf("gc.batch_size = %d", gc.BatchSize)
	}
	if gc.Concurrency != 4 {
		t.Errorf("gc.concurrency = %d", gc.Concurrency)
	}
	if gc.Shards != 8 {
		t.Errorf("gc.shards = %d", gc.Shards)
	}
	if !gc.DualWrite {
		t.Errorf("gc.dual_write = %v", gc.DualWrite)
	}
	if gc.MetricsListen != ":9300" {
		t.Errorf("gc.metrics_listen = %q", gc.MetricsListen)
	}

	lc := cfg.Workers.Lifecycle
	if lc.Interval != 13*time.Second {
		t.Errorf("lifecycle.interval = %v", lc.Interval)
	}
	if lc.Unit != "hour" {
		t.Errorf("lifecycle.unit = %q", lc.Unit)
	}
	if lc.Concurrency != 3 {
		t.Errorf("lifecycle.concurrency = %d", lc.Concurrency)
	}
	if lc.MetricsListen != ":9301" {
		t.Errorf("lifecycle.metrics_listen = %q", lc.MetricsListen)
	}

	rb := cfg.Workers.Rebalance
	if rb.Interval != 7*time.Minute {
		t.Errorf("rebalance.interval = %v", rb.Interval)
	}
	if rb.RateMBPerS != 250 {
		t.Errorf("rebalance.rate_mb_s = %d", rb.RateMBPerS)
	}
	if rb.Inflight != 16 {
		t.Errorf("rebalance.inflight = %d", rb.Inflight)
	}
	if rb.Shards != 4 {
		t.Errorf("rebalance.shards = %d", rb.Shards)
	}

	ur := cfg.Workers.UsageRollup
	if ur.At != "02:30" {
		t.Errorf("usage_rollup.at = %q", ur.At)
	}
	if ur.Interval != 12*time.Hour {
		t.Errorf("usage_rollup.interval = %v", ur.Interval)
	}
	if ur.SamplesPerDay != 48 {
		t.Errorf("usage_rollup.samples_per_day = %d", ur.SamplesPerDay)
	}

	mr := cfg.Workers.ManifestRewriter
	if mr.Interval != 30*time.Minute {
		t.Errorf("manifest_rewriter.interval = %v", mr.Interval)
	}
	if mr.BatchLimit != 1000 {
		t.Errorf("manifest_rewriter.batch_limit = %d", mr.BatchLimit)
	}
	if !mr.DryRun {
		t.Errorf("manifest_rewriter.dry_run = %v", mr.DryRun)
	}

	ae := cfg.Workers.AuditExport
	if ae.Bucket != "audit-archive" {
		t.Errorf("audit_export.bucket = %q", ae.Bucket)
	}
	if ae.Prefix != "exports/" {
		t.Errorf("audit_export.prefix = %q", ae.Prefix)
	}
	if ae.After != 168*time.Hour {
		t.Errorf("audit_export.after = %v", ae.After)
	}
	if ae.Interval != 1*time.Hour {
		t.Errorf("audit_export.interval = %v", ae.Interval)
	}

	if cfg.Workers.QuotaReconcile.Interval != 30*time.Minute {
		t.Errorf("quota_reconcile.interval = %v", cfg.Workers.QuotaReconcile.Interval)
	}

	n := cfg.Workers.Notify
	if n.Targets != "primary=https://example.com/hook|secret" {
		t.Errorf("notify.targets = %q", n.Targets)
	}
	if n.Interval != 9*time.Second {
		t.Errorf("notify.interval = %v", n.Interval)
	}
	if n.MaxRetries != 3 {
		t.Errorf("notify.max_retries = %d", n.MaxRetries)
	}
	if n.BackoffBase != 750*time.Millisecond {
		t.Errorf("notify.backoff_base = %v", n.BackoffBase)
	}
	if n.PollLimit != 42 {
		t.Errorf("notify.poll_limit = %d", n.PollLimit)
	}

	r := cfg.Workers.Replicator
	if r.Interval != 9*time.Second {
		t.Errorf("replicator.interval = %v", r.Interval)
	}
	if r.MaxRetries != 3 {
		t.Errorf("replicator.max_retries = %d", r.MaxRetries)
	}
	if r.BackoffBase != 750*time.Millisecond {
		t.Errorf("replicator.backoff_base = %v", r.BackoffBase)
	}
	if r.PollLimit != 42 {
		t.Errorf("replicator.poll_limit = %d", r.PollLimit)
	}
	if r.HTTPTimeout != 11*time.Second {
		t.Errorf("replicator.http_timeout = %v", r.HTTPTimeout)
	}
	if r.PeerScheme != "http" {
		t.Errorf("replicator.peer_scheme = %q", r.PeerScheme)
	}

	al := cfg.Workers.AccessLog
	if al.Interval != 2*time.Minute {
		t.Errorf("access_log.interval = %v", al.Interval)
	}
	if al.MaxFlushBytes != 12345 {
		t.Errorf("access_log.max_flush_bytes = %d", al.MaxFlushBytes)
	}
	if al.PollLimit != 99 {
		t.Errorf("access_log.poll_limit = %d", al.PollLimit)
	}

	inv := cfg.Workers.Inventory
	if inv.Interval != 3*time.Minute {
		t.Errorf("inventory.interval = %v", inv.Interval)
	}
	if inv.Region != "eu-west-1" {
		t.Errorf("inventory.region = %q", inv.Region)
	}
}

// TestEnvOverridesTOMLForWorkers pins the env > TOML precedence required
// by the PRD's backward-compat acceptance. A worker knob set via env MUST
// win over the same key in TOML; this test exercises one knob per worker
// substruct so a future koanf provider-ordering regression breaks here.
func TestEnvOverridesTOMLForWorkers(t *testing.T) {
	clearEnv(t)

	body := `
[workers.gc]
interval = "11s"
[workers.lifecycle]
interval = "13s"
[workers.rebalance]
interval = "7m"
[workers.access_log]
interval = "2m"
`
	dir := t.TempDir()
	path := filepath.Join(dir, "strata.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write toml: %v", err)
	}
	t.Setenv("STRATA_CONFIG_FILE", path)
	t.Setenv("STRATA_GC_INTERVAL", "99s")
	t.Setenv("STRATA_LIFECYCLE_INTERVAL", "55s")
	t.Setenv("STRATA_REBALANCE_INTERVAL", "20m")
	t.Setenv("STRATA_ACCESS_LOG_INTERVAL", "10m")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Workers.GC.Interval != 99*time.Second {
		t.Errorf("gc.interval env override: %v want 99s", cfg.Workers.GC.Interval)
	}
	if cfg.Workers.Lifecycle.Interval != 55*time.Second {
		t.Errorf("lifecycle.interval env override: %v want 55s", cfg.Workers.Lifecycle.Interval)
	}
	if cfg.Workers.Rebalance.Interval != 20*time.Minute {
		t.Errorf("rebalance.interval env override: %v want 20m", cfg.Workers.Rebalance.Interval)
	}
	if cfg.Workers.AccessLog.Interval != 10*time.Minute {
		t.Errorf("access_log.interval env override: %v want 10m", cfg.Workers.AccessLog.Interval)
	}
}

// TestClampWorkersOnLoad pins the symmetry between TOML-loaded values and
// env-loaded values for clamp ranges. An out-of-range value in TOML gets
// the same clamp + WARN log as one set via env.
func TestClampWorkersOnLoad(t *testing.T) {
	clearEnv(t)

	body := `
[workers.gc]
concurrency = 999
shards = 9999

[workers.rebalance]
interval = "48h"
rate_mb_s = 0
inflight = 1000
shards = 9999
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
	if cfg.Workers.GC.Concurrency != 256 {
		t.Errorf("gc.concurrency clamp: %d want 256", cfg.Workers.GC.Concurrency)
	}
	if cfg.Workers.GC.Shards != 1024 {
		t.Errorf("gc.shards clamp: %d want 1024", cfg.Workers.GC.Shards)
	}
	if cfg.Workers.Rebalance.Interval != 24*time.Hour {
		t.Errorf("rebalance.interval clamp: %v want 24h", cfg.Workers.Rebalance.Interval)
	}
	if cfg.Workers.Rebalance.RateMBPerS != 1 {
		t.Errorf("rebalance.rate_mb_s clamp: %d want 1", cfg.Workers.Rebalance.RateMBPerS)
	}
	if cfg.Workers.Rebalance.Inflight != 64 {
		t.Errorf("rebalance.inflight clamp: %d want 64", cfg.Workers.Rebalance.Inflight)
	}
	if cfg.Workers.Rebalance.Shards != 1024 {
		t.Errorf("rebalance.shards clamp: %d want 1024", cfg.Workers.Rebalance.Shards)
	}
}

// TestDefaultWorkerKnobs pins every default carried in defaults() so the
// drift-lint test in US-006 has a stable baseline for the example TOML
// comments.
func TestDefaultWorkerKnobs(t *testing.T) {
	clearEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := defaults().Workers
	if got := cfg.Workers; got != want {
		// Render specific deltas — full struct dump is noisy.
		if got.GC != want.GC {
			t.Errorf("Workers.GC default = %+v want %+v", got.GC, want.GC)
		}
		if got.Lifecycle != want.Lifecycle {
			t.Errorf("Workers.Lifecycle default = %+v want %+v", got.Lifecycle, want.Lifecycle)
		}
		if got.Rebalance != want.Rebalance {
			t.Errorf("Workers.Rebalance default = %+v want %+v", got.Rebalance, want.Rebalance)
		}
		if got.UsageRollup != want.UsageRollup {
			t.Errorf("Workers.UsageRollup default = %+v want %+v", got.UsageRollup, want.UsageRollup)
		}
		if got.ManifestRewriter != want.ManifestRewriter {
			t.Errorf("Workers.ManifestRewriter default = %+v want %+v", got.ManifestRewriter, want.ManifestRewriter)
		}
		if got.AuditExport != want.AuditExport {
			t.Errorf("Workers.AuditExport default = %+v want %+v", got.AuditExport, want.AuditExport)
		}
		if got.QuotaReconcile != want.QuotaReconcile {
			t.Errorf("Workers.QuotaReconcile default = %+v want %+v", got.QuotaReconcile, want.QuotaReconcile)
		}
		if got.Notify != want.Notify {
			t.Errorf("Workers.Notify default = %+v want %+v", got.Notify, want.Notify)
		}
		if got.Replicator != want.Replicator {
			t.Errorf("Workers.Replicator default = %+v want %+v", got.Replicator, want.Replicator)
		}
		if got.AccessLog != want.AccessLog {
			t.Errorf("Workers.AccessLog default = %+v want %+v", got.AccessLog, want.AccessLog)
		}
		if got.Inventory != want.Inventory {
			t.Errorf("Workers.Inventory default = %+v want %+v", got.Inventory, want.Inventory)
		}
	}
}
