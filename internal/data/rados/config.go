package rados

import (
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/trace"
)

type Config struct {
	ConfigFile string
	User       string
	Keyring    string
	Pool       string
	Namespace  string
	Classes    map[string]ClassSpec
	// Clusters is the per-cluster connection map keyed by ClusterSpec.ID.
	// Empty map means "use the legacy single-cluster fields above as the
	// 'default' cluster"; see BuildClusters for the resolution rules.
	Clusters map[string]ClusterSpec
	// Logger receives DEBUG lines per RADOS op (read/write/delete) when set.
	Logger *slog.Logger
	// Metrics receives one ObserveOp call per RADOS op. Cmd-layer plugs
	// metrics.RADOSObserver{}; nil disables.
	Metrics Metrics
	// Tracer, when set, emits one OTel child span per RADOS op. Cmd-layer
	// plugs tracerProvider.Tracer("strata.data.rados"); nil disables.
	Tracer trace.Tracer
	// Catalog supplies the cluster_registry rows the RegistryWatcher
	// polls for. Set by internal/serverapp from the live meta.Store via a
	// thin adapter (the rados package cannot import internal/meta — cycle).
	// Nil disables the watcher and falls back to the legacy Clusters map.
	Catalog CatalogReader
	// RegistryMetrics receives one IncRegistryChange call per watcher
	// reconciliation. Cmd-layer plugs metrics.RADOSRegistryObserver{}; nil
	// disables.
	RegistryMetrics RegistryMetrics
}

// Metrics is the narrow interface RADOS observers implement. The cmd binary
// supplies metrics.RADOSObserver{}; internal package stays free of
// prometheus.
type Metrics interface {
	ObserveOp(pool, op string, duration time.Duration, err error)
}
