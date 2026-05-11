package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/push"

	"github.com/danchupin/strata/internal/config"
	"github.com/danchupin/strata/internal/data"
	datamem "github.com/danchupin/strata/internal/data/memory"
	datarados "github.com/danchupin/strata/internal/data/rados"
)

// buildBenchDataBackend mirrors serverapp.buildDataBackend for the bench
// subcommands. Only the memory + rados shapes are wired — bench harness has
// no need for s3 (US-014 lifecycle-translation backend) since the bench is
// strictly about chunk-delete / per-object-action throughput in the worker.
//
// Without the `ceph` build tag rados.New returns an error from the stub, so
// running bench against a rados-configured strata-admin requires the same
// ceph-tagged binary the gateway uses.
func buildBenchDataBackend(cfg *config.Config, logger *slog.Logger) (data.Backend, error) {
	switch cfg.DataBackend {
	case "memory":
		return datamem.New(), nil
	case "rados":
		classes, err := datarados.ParseClasses(cfg.RADOS.Classes)
		if err != nil {
			return nil, err
		}
		return datarados.New(datarados.Config{
			Pool:      cfg.RADOS.Pool,
			Namespace: cfg.RADOS.Namespace,
			Classes:   classes,
			Logger:    logger,
		})
	default:
		return nil, fmt.Errorf("bench: unsupported data backend %q (memory or rados)", cfg.DataBackend)
	}
}

// benchResult is the JSON-line shape both bench subcommands print on stdout.
// Field names match the strata_*_bench_throughput Prometheus gauge labels so
// downstream dashboards can correlate the JSONL artifacts with scraped data.
//
// `shards` (US-006 Phase 2) defaults to 1 — single-leader Phase 1 shape — and
// is set to N>1 by the multi-leader bench mode where N parallel workers drain
// disjoint shard slices in one process.
type benchResult struct {
	Bench         string  `json:"bench"`
	Entries       int     `json:"entries"`
	Concurrency   int     `json:"concurrency"`
	Shards        int     `json:"shards,omitempty"`
	ElapsedMs     int64   `json:"elapsed_ms"`
	ThroughputPS  float64 `json:"throughput_per_sec"`
	MetaBackend   string  `json:"meta_backend"`
	DataBackend   string  `json:"data_backend"`
	StartedAtUnix int64   `json:"started_at_unix"`
}

func newBenchResult(bench string, entries, concurrency int, elapsed time.Duration, cfg *config.Config, started time.Time) benchResult {
	tput := 0.0
	if elapsed > 0 {
		tput = float64(entries) / elapsed.Seconds()
	}
	return benchResult{
		Bench:         bench,
		Entries:       entries,
		Concurrency:   concurrency,
		ElapsedMs:     elapsed.Milliseconds(),
		ThroughputPS:  tput,
		MetaBackend:   cfg.MetaBackend,
		DataBackend:   cfg.DataBackend,
		StartedAtUnix: started.Unix(),
	}
}

// pushBenchGauge publishes throughput_per_sec to STRATA_PROM_PUSHGATEWAY when
// set. No-op when unset (bench remains usable in lab without a push gateway).
// Failures log WARN and return nil — push-gateway outages must never fail the
// bench run.
func pushBenchGauge(ctx context.Context, logger *slog.Logger, metricName, job string, res benchResult) error {
	target := strings.TrimSpace(os.Getenv("STRATA_PROM_PUSHGATEWAY"))
	if target == "" {
		return nil
	}
	g := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: metricName,
		Help: "Throughput (entries/sec) measured by the strata-admin bench harness.",
		ConstLabels: prometheus.Labels{
			"concurrency":  fmt.Sprintf("%d", res.Concurrency),
			"meta_backend": res.MetaBackend,
			"data_backend": res.DataBackend,
		},
	})
	g.Set(res.ThroughputPS)
	pusher := push.New(target, job).Collector(g).Client(&http.Client{Timeout: 5 * time.Second})
	if err := pusher.PushContext(ctx); err != nil {
		logger.WarnContext(ctx, "bench: prometheus push failed", "endpoint", target, "error", err.Error())
		return nil
	}
	return nil
}

