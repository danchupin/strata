package cassandra

import (
	"testing"

	"github.com/danchupin/strata/internal/meta"
)

// TestStoreDoesNotImplementRangeScanStore is the load-bearing negative
// assertion for US-012. cassandra.Store deliberately stays out of
// meta.RangeScanStore because its objects table is partitioned by
// (bucket_id, shard) — any prefix scan must fan out across N shard
// partitions and heap-merge by clustering order, and that fan-out IS the
// implementation in Store.ListObjects. Hoisting it under a "single ordered
// range scan" name would just rename the same code. The gateway dispatch
// site (internal/s3api/server.go::listObjects) relies on this assertion
// failing for cassandra so the fan-out path stays load-bearing.
func TestStoreDoesNotImplementRangeScanStore(t *testing.T) {
	var s *Store
	if _, ok := any(s).(meta.RangeScanStore); ok {
		t.Fatal("cassandra.Store must NOT implement meta.RangeScanStore — see the type-comment on Store and US-012 design notes")
	}
}
