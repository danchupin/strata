//go:build integration

package cassandra_test

import (
	"context"
	"os"
	"testing"

	"github.com/testcontainers/testcontainers-go"
	tccassandra "github.com/testcontainers/testcontainers-go/modules/cassandra"
)

// Shared Cassandra testcontainer setup for every integration test in this
// package (US-010). Centralising the boot here gives two things at once:
//   - a single pinned image + JVM tuning so the LWT/Paxos contract stays green
//     on a CPU-starved CI runner instead of hanging for the full gocql timeout;
//   - one place to refactor (CLAUDE.md "reuse harnesses — don't fork").

// cassandraImage is the single pinned Cassandra image used by the whole
// integration suite. Override via STRATA_CASSANDRA_IMAGE (the dedicated CI job
// can bump the tag without touching code).
func cassandraImage() string {
	return envOr("STRATA_CASSANDRA_IMAGE", "cassandra:5.0")
}

// cassandraTuningOpts returns the JVM/heap customizers that keep Cassandra's
// LWT/Paxos path responsive on a resource-constrained CI runner — the
// root-cause work behind US-010's per-PR-runner starvation.
//
// The testcontainers cassandra module defaults to MAX_HEAP_SIZE=1024M /
// HEAP_NEWSIZE=128M. On a 2-vCPU GitHub runner the tiny new-gen forces
// frequent young-GC pauses; under those pauses the single-node coordinator
// can't finish Paxos prepare/propose rounds before gocql's 60s timeout, so the
// contract's heavy LWT path (CreateBucket, SetBucketVersioning, multipart
// Complete, GC fan-out) "hangs" where it passes in ~17s on a dev box. Pinning a
// larger, deterministic heap with a fatter new-gen removes the GC thrash.
// Knobs stay env-overridable so a beefier dedicated runner can raise them
// further with no code change.
//
// Applied AFTER the module's own WithEnv (Run appends caller opts last), and
// testcontainers.WithEnv merges via maps.Copy, so these keys override the
// module defaults rather than dropping the snitch/DC settings.
func cassandraTuningOpts() []testcontainers.ContainerCustomizer {
	return []testcontainers.ContainerCustomizer{
		testcontainers.WithEnv(map[string]string{
			"MAX_HEAP_SIZE": envOr("STRATA_CASSANDRA_MAX_HEAP", "2048M"),
			"HEAP_NEWSIZE":  envOr("STRATA_CASSANDRA_NEW_HEAP", "512M"),
		}),
	}
}

// startCassandra boots a tuned single-node Cassandra container and returns its
// CQL connection host. The container is terminated on test/bench cleanup. Use
// this instead of calling tccassandra.Run directly so every contract/reshard
// test inherits the same CI-safe JVM tuning (US-010). Accepts testing.TB so
// both *testing.T tests and the *testing.B bench share it.
func startCassandra(tb testing.TB) string {
	tb.Helper()
	ctx := context.Background()
	container, err := tccassandra.Run(ctx, cassandraImage(), cassandraTuningOpts()...)
	if err != nil {
		tb.Fatalf("start cassandra: %v", err)
	}
	tb.Cleanup(func() {
		if err := container.Terminate(context.Background()); err != nil {
			tb.Logf("terminate: %v", err)
		}
	})
	host, err := container.ConnectionHost(ctx)
	if err != nil {
		tb.Fatalf("connection host: %v", err)
	}
	return host
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
