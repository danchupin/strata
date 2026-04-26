// strata-notify drains the notify_queue and dispatches each event to its
// configured sink (webhook in US-009; SQS in US-010). Leader-elected via
// internal/leader so only one notify worker is active per Strata cluster.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/danchupin/strata/internal/config"
	"github.com/danchupin/strata/internal/leader"
	"github.com/danchupin/strata/internal/meta"
	metacassandra "github.com/danchupin/strata/internal/meta/cassandra"
	metamem "github.com/danchupin/strata/internal/meta/memory"
	"github.com/danchupin/strata/internal/metrics"
	"github.com/danchupin/strata/internal/notify"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: parseLogLevel(os.Getenv("STRATA_LOG_LEVEL"))}))
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		logger.Error("config", "error", err.Error())
		os.Exit(2)
	}

	router, err := notify.RouterFromEnv(notify.WithSQSClientFactory(sqsClientFactory))
	if err != nil {
		logger.Error("notify router",
			"error", err.Error(),
			"hint", "set STRATA_NOTIFY_TARGETS=<type>:<arn>=<url>|<secret>,...")
		os.Exit(2)
	}

	store, err := buildMetaStore(cfg, logger)
	if err != nil {
		logger.Error("meta store", "error", err.Error())
		os.Exit(2)
	}
	defer store.Close()

	w, err := notify.New(notify.Config{
		Meta:        store,
		Router:      router,
		Logger:      logger,
		Interval:    parseDuration("STRATA_NOTIFY_INTERVAL", 5*time.Second),
		MaxRetries:  parseInt("STRATA_NOTIFY_MAX_RETRIES", 6),
		BackoffBase: parseDuration("STRATA_NOTIFY_BACKOFF_BASE", 1*time.Second),
		PollLimit:   parseInt("STRATA_NOTIFY_POLL_LIMIT", 100),
	})
	if err != nil {
		logger.Error("worker", "error", err.Error())
		os.Exit(2)
	}

	metrics.Register()
	go func() {
		listen := envOr("STRATA_NOTIFY_METRICS_LISTEN", ":9102")
		mux := http.NewServeMux()
		mux.Handle("/metrics", metrics.Handler())
		logger.Info("notify: metrics", "listen", listen)
		_ = http.ListenAndServe(listen, mux)
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	locker := buildLocker(cfg, store)
	if locker == nil {
		logger.Warn("notify: leader election disabled (no distributed locker for this meta backend)",
			"meta_backend", cfg.MetaBackend)
		if err := w.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("worker", "error", err.Error())
			os.Exit(1)
		}
		return
	}
	session := &leader.Session{Locker: locker, Name: "notify", Holder: leader.DefaultHolder()}
	for ctx.Err() == nil {
		if err := session.AwaitAcquire(ctx); err != nil {
			return
		}
		workCtx := session.Supervise(ctx)
		if err := w.Run(workCtx); err != nil && !errors.Is(err, context.Canceled) {
			logger.Warn("worker", "error", err.Error())
		}
		session.Release(context.Background())
	}
}

func buildLocker(cfg *config.Config, store meta.Store) leader.Locker {
	if cfg.MetaBackend == "cassandra" {
		if cs, ok := store.(*metacassandra.Store); ok {
			return &metacassandra.Locker{S: cs.Session()}
		}
	}
	if cfg.MetaBackend == "memory" {
		if ms, ok := store.(*metamem.Store); ok {
			return ms.Locker()
		}
	}
	return nil
}

func buildMetaStore(cfg *config.Config, logger *slog.Logger) (meta.Store, error) {
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
				Metrics:     metrics.CassandraObserver{},
			},
			metacassandra.Options{DefaultShardCount: cfg.DefaultBucketShards},
		)
	default:
		return nil, errors.New("unknown meta backend: " + cfg.MetaBackend)
	}
}

// sqsClientFactory builds an AWS SDK SQS client per RouterFromEnv target.
// Resolves credentials via the standard AWS chain (env vars, shared config,
// IRSA, EC2/ECS instance roles). The optional region argument overrides the
// SDK default; the empty string lets the chain pick the region from
// AWS_REGION / EC2 metadata.
func sqsClientFactory(region string) (notify.SQSAPI, error) {
	ctx := context.Background()
	var opts []func(*awsconfig.LoadOptions) error
	if region != "" {
		opts = append(opts, awsconfig.WithRegion(region))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, err
	}
	return sqs.NewFromConfig(cfg), nil
}

func parseLogLevel(s string) slog.Level {
	switch s {
	case "DEBUG", "debug":
		return slog.LevelDebug
	case "WARN", "warn":
		return slog.LevelWarn
	case "ERROR", "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func parseDuration(name string, def time.Duration) time.Duration {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}

func parseInt(name string, def int) int {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}
