package config

import (
	"slices"
	"strings"
)

// ExemptEnvVars enumerates STRATA_* environment variables that intentionally
// do NOT have a corresponding field in Config / koanf TOML key, and therefore
// MUST be skipped by the audit script (scripts/audit-env-toml-parity.sh) and
// the drift-lint test (internal/config/env_toml_parity_test.go).
//
// Categories:
//   - Bootstrap: STRATA_CONFIG_FILE points AT the TOML file; can't itself
//     be IN the TOML.
//   - Retired: STRATA_DRAIN_STRICT is honored only by warnLegacyDrainStrict
//     to log a WARN. No struct field; left for graceful operator migration.
//   - Build metadata: STRATA_VERSION is read once at boot to override the
//     built-in version string in `strata version`; not a runtime knob.
//   - Test-only: STRATA_STORAGE_HEALTH_OVERRIDE (admin storage handler test
//     hook), STRATA_SCYLLA_* (Scylla integration suite toggles),
//     STRATA_REGEN_TRAILER_FIXTURES (one-shot fixture regenerator),
//     STRATA_TEST_* / STRATA_BENCH_* families.
//   - CLI client convenience: STRATA_ADMIN_ENDPOINT, STRATA_ADMIN_PRINCIPAL
//     (defaults for the `strata admin` CLI flags, not server config).
//   - Bench output: STRATA_PROM_PUSHGATEWAY (push target for bench gauges).
//   - Test-sentinel suffixes: *_TEST, *_TEST_ABSENT/HI/LO, *_UNSET,
//     *_INT64_TEST, *_BOOL_TEST.
//
// External (non-STRATA_) bootstrap envs (e.g. OTEL_EXPORTER_OTLP_ENDPOINT)
// are out of scope — the audit only inspects STRATA_-prefixed names.
var ExemptEnvVars = struct {
	Exact    []string
	Prefixes []string
	Suffixes []string
}{
	Exact: []string{
		"STRATA_CONFIG_FILE",
		"STRATA_REGEN_TRAILER_FIXTURES",
		"STRATA_DRAIN_STRICT",
		"STRATA_VERSION",
		"STRATA_STORAGE_HEALTH_OVERRIDE",
		"STRATA_SCYLLA_IMAGE",
		"STRATA_SCYLLA_TEST",
		"STRATA_ADMIN_ENDPOINT",
		"STRATA_ADMIN_PRINCIPAL",
		"STRATA_PROM_PUSHGATEWAY",
		// STRATA_ALERTMANAGER_URL defaults the --alertmanager-url flag on
		// the `strata admin slo-report` subcommand (US-005). CLI client
		// convenience — not server config. STRATA_PROMETHEUS_URL is NOT
		// exempt; it backs `prometheus.url` via PrometheusConfig.
		"STRATA_ALERTMANAGER_URL",
		// STRATA_PPROF_SMOKE_PROFILE points the TestPprofDecode entrypoint at
		// a captured pprof file for scripts/smoke-pprof.sh — test-only hook,
		// no runtime config knob.
		"STRATA_PPROF_SMOKE_PROFILE",
		"STRATA_PPROF_SKIP_TOOL_CHECK",
		"STRATA_TIKV_TEST_PD_ENDPOINTS",
		// STRATA_COLD is a storage-class identifier used in S3 lifecycle
		// rules, not an environment variable. The audit's broad
		// quoted-literal collector picks it up — exempt it explicitly.
		"STRATA_COLD",
	},
	Prefixes: []string{
		"STRATA_BENCH_",
		"STRATA_TEST_",
	},
	Suffixes: []string{
		"_TEST",
		"_TEST_ABSENT",
		"_TEST_HI",
		"_TEST_LO",
		"_UNSET",
		"_INT64_TEST",
		"_BOOL_TEST",
	},
}

// IsExempt reports whether the env var should be skipped by the audit /
// drift-lint check.
func IsExempt(name string) bool {
	if slices.Contains(ExemptEnvVars.Exact, name) {
		return true
	}
	for _, p := range ExemptEnvVars.Prefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	for _, s := range ExemptEnvVars.Suffixes {
		if strings.HasSuffix(name, s) {
			return true
		}
	}
	return false
}
