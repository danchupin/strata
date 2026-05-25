package workers

import (
	"net/http"
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
	cfg := workerCfg(deps)
	rCfg := cfg.Workers.Replicator
	dispatcher := &replication.HTTPDispatcher{
		Client: &http.Client{Timeout: orDuration(rCfg.HTTPTimeout, 30*time.Second)},
		Scheme: orString(rCfg.PeerScheme, "https"),
	}
	return replication.New(replication.Config{
		Meta:        deps.Meta,
		Data:        deps.Data,
		Dispatcher:  dispatcher,
		Logger:      deps.Logger,
		Metrics:     metrics.ReplicationObserver{},
		Interval:    orDuration(rCfg.Interval, 5*time.Second),
		MaxRetries:  orInt(rCfg.MaxRetries, 6),
		BackoffBase: orDuration(rCfg.BackoffBase, 1*time.Second),
		PollLimit:   orInt(rCfg.PollLimit, 100),
		Tracer:      deps.Tracer.Tracer("strata.worker.replicator"),
	})
}
