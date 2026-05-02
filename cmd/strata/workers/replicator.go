package workers

import (
	"net/http"
	"os"
	"time"

	"github.com/danchupin/strata/internal/metrics"
	"github.com/danchupin/strata/internal/replication"
)

func init() {
	Register(Worker{
		Name:  "replicator",
		Build: buildReplicator,
	})
}

func buildReplicator(deps Dependencies) (Runner, error) {
	dispatcher := &replication.HTTPDispatcher{
		Client: &http.Client{Timeout: durationFromEnv("STRATA_REPLICATOR_HTTP_TIMEOUT", 30*time.Second)},
		Scheme: stringFromEnv("STRATA_REPLICATOR_PEER_SCHEME", "https"),
	}
	return replication.New(replication.Config{
		Meta:        deps.Meta,
		Data:        deps.Data,
		Dispatcher:  dispatcher,
		Logger:      deps.Logger,
		Metrics:     metrics.ReplicationObserver{},
		Interval:    durationFromEnv("STRATA_REPLICATOR_INTERVAL", 5*time.Second),
		MaxRetries:  intFromEnv("STRATA_REPLICATOR_MAX_RETRIES", 6),
		BackoffBase: durationFromEnv("STRATA_REPLICATOR_BACKOFF_BASE", 1*time.Second),
		PollLimit:   intFromEnv("STRATA_REPLICATOR_POLL_LIMIT", 100),
	})
}

func stringFromEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
