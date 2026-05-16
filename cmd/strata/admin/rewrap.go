package admin

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/danchupin/strata/internal/config"
	"github.com/danchupin/strata/internal/crypto/master"
	"github.com/danchupin/strata/internal/meta"
	metacassandra "github.com/danchupin/strata/internal/meta/cassandra"
	metamem "github.com/danchupin/strata/internal/meta/memory"
	metatikv "github.com/danchupin/strata/internal/meta/tikv"
	"github.com/danchupin/strata/internal/rewrap"
)

// cmdRewrap runs the SSE master-key rotation rewrap pass against the
// configured meta backend, mirroring what cmd/strata-rewrap did before
// US-013. Resumption is automatic via the persisted per-bucket
// RewrapProgress rows; there is no --continue / --from-bucket flag.
func (a *app) cmdRewrap(ctx context.Context, jsonOut bool, args []string) error {
	fs := flag.NewFlagSet("rewrap", flag.ContinueOnError)
	fs.SetOutput(a.err)
	target := fs.String("target-key-id", "", "destination wrap key id (defaults to first STRATA_SSE_MASTER_KEYS entry)")
	dryRun := fs.Bool("dry-run", false, "scan only; do not rewrap")
	batch := fs.Int("batch", 500, "page size for ListObjectVersions / ListMultipartUploads")
	if err := fs.Parse(args); err != nil {
		return ErrUsage
	}

	logger := slog.New(slog.NewJSONHandler(a.err, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	provider, err := master.NewRotationProviderFromEnv()
	if err != nil {
		return fmt.Errorf("rotation provider: %w (hint: set STRATA_SSE_MASTER_KEYS=<id>:<hex64>[,<id>:<hex64>...])", err)
	}
	if *target != "" && *target != provider.ActiveID() {
		provider, err = reorderProvider(provider, *target)
		if err != nil {
			return err
		}
	}
	logger.Info("rewrap starting",
		"active_id", provider.ActiveID(),
		"all_ids", provider.IDs(),
		"dry_run", *dryRun,
	)

	store, err := buildRewrapMetaStore(cfg, logger)
	if err != nil {
		return fmt.Errorf("meta store: %w", err)
	}
	defer store.Close()

	if *dryRun {
		logger.Info("dry-run requested; counting only (no UPDATE issued)")
	}

	w, err := rewrap.New(rewrap.Config{
		Meta:       store,
		Provider:   provider,
		Logger:     logger,
		BatchLimit: *batch,
	})
	if err != nil {
		return fmt.Errorf("worker: %w", err)
	}

	runCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	stats, err := w.Run(runCtx)
	if err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("rewrap run failed",
			"error", err.Error(),
			"buckets_scanned", stats.BucketsScanned,
			"objects_rewrapped", stats.ObjectsRewrapped,
			"uploads_rewrapped", stats.UploadsRewrapped,
		)
		return fmt.Errorf("rewrap run failed: %w", err)
	}
	logger.Info("rewrap complete",
		"buckets_scanned", stats.BucketsScanned,
		"buckets_skipped", stats.BucketsSkipped,
		"objects_scanned", stats.ObjectsScanned,
		"objects_rewrapped", stats.ObjectsRewrapped,
		"uploads_scanned", stats.UploadsScanned,
		"uploads_rewrapped", stats.UploadsRewrapped,
	)
	if jsonOut {
		return writeJSON(a.out, stats)
	}
	fmt.Fprintf(a.out, "rewrap: active=%s\n", provider.ActiveID())
	fmt.Fprintf(a.out, "  buckets:  scanned=%d skipped=%d\n", stats.BucketsScanned, stats.BucketsSkipped)
	fmt.Fprintf(a.out, "  objects:  scanned=%d rewrapped=%d\n", stats.ObjectsScanned, stats.ObjectsRewrapped)
	fmt.Fprintf(a.out, "  uploads:  scanned=%d rewrapped=%d\n", stats.UploadsScanned, stats.UploadsRewrapped)
	return nil
}

// reorderProvider returns a new RotationProvider whose active id is target.
// target must already exist in p's rotation list.
func reorderProvider(p *master.RotationProvider, target string) (*master.RotationProvider, error) {
	entries := p.Entries()
	idx := -1
	for i, e := range entries {
		if e.ID == target {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil, fmt.Errorf("--target-key-id %q not in STRATA_SSE_MASTER_KEYS (have %v)", target, p.IDs())
	}
	if idx > 0 {
		entries[0], entries[idx] = entries[idx], entries[0]
	}
	return master.NewRotationProvider(entries)
}

func buildRewrapMetaStore(cfg *config.Config, logger *slog.Logger) (meta.Store, error) {
	switch cfg.MetaBackend {
	case "memory":
		return metamem.New(), nil
	case "cassandra":
		return metacassandra.Open(
			metacassandra.SessionConfig{
				Hosts:       cfg.Cassandra.Hosts,
				Keyspace:    cfg.Cassandra.Keyspace,
				LocalDC:     cfg.Cassandra.LocalDC,
				Replication: cfg.Cassandra.Replication,
				Username:    cfg.Cassandra.Username,
				Password:    cfg.Cassandra.Password,
				Timeout:     cfg.Cassandra.Timeout,
				Logger:      logger,
				SlowMS:      metacassandra.SlowMSFromEnv(),
			},
			metacassandra.Options{DefaultShardCount: cfg.DefaultBucketShards},
		)
	case "tikv":
		eps := splitTiKVEndpoints(cfg.TiKV.Endpoints)
		if len(eps) == 0 {
			return nil, errors.New("tikv: STRATA_TIKV_PD_ENDPOINTS is empty")
		}
		return metatikv.Open(metatikv.Config{PDEndpoints: eps})
	default:
		return nil, fmt.Errorf("unknown meta backend: %s", cfg.MetaBackend)
	}
}

func splitTiKVEndpoints(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}
