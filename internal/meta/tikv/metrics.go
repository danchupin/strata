package tikv

// Metrics is the optional observability sink the Store consumes. Implementations
// live in the cmd-layer (internal/metrics.TiKVObserver) so this package stays
// free of a prometheus import — same convention as cassandra.Metrics and
// rados.Metrics.
//
// Methods are best-effort signals; nil sinks are no-ops at the call site.
type Metrics interface {
	// IncBucketStatsShardWrite bumps strata_bucket_stats_shard_writes_total{shard}
	// once per successful BumpBucketStats commit. Operators read uniform
	// distribution across shards as healthy; one shard dominating points at a
	// pickBucketStatsShard hash-collision pattern or a degenerate caller
	// (e.g. hashing on a stable key instead of a fresh uuid).
	IncBucketStatsShardWrite(shard int)
	// IncPessimisticTxn bumps strata_tikv_pessimistic_txn_total{op, outcome}
	// once per terminal pessimistic-txn outcome (US-001 cycle B prod-
	// observability). outcome ∈ {commit, rollback, conflict}. op is the
	// Store method name stashed in ctx by observer.Start.
	IncPessimisticTxn(op, outcome string)
}
