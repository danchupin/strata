//go:build integration

package tikv

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/go-connections/nat"
	"github.com/testcontainers/testcontainers-go"
	tcnetwork "github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
	"github.com/tikv/client-go/v2/txnkv"

	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/meta/storetest"
)

// TestTiKVStoreContract runs the shared meta.Store contract suite against a
// real PD + TiKV pair.
//
// Cluster sourcing (in priority order):
//
//  1. STRATA_TIKV_TEST_PD_ENDPOINTS — comma-separated PD client addresses.
//     Operator-provided cluster; tests run unconditionally against it.
//     CI workflows (US-017) supply this so the suite exercises whatever
//     PD/TiKV the workflow already brought up via docker-compose.
//
//  2. Otherwise: spawn pingcap/pd + pingcap/tikv via testcontainers-go on a
//     private docker network with the host-gateway advertise pattern (see
//     setUpTiKVCluster below). Suitable for local `make test-integration`.
//
// If both paths fail (no env, no Docker, image pull error), the test
// t.Skipf's with the underlying error so a sandboxed CI runner without
// container support still marks the suite passing — the contract surface is
// already validated against the in-process memory backend (parity oracle).
//
// The cluster is shared across subtests; contract cases use distinct bucket
// names per case so writes do not collide. A fresh `go test` invocation
// brings up a fresh cluster.
//
// Runs only under `go test -tags integration`.
func TestTiKVStoreContract(t *testing.T) {
	ctx := context.Background()

	endpoints, cleanup := acquireCluster(ctx, t)
	t.Cleanup(cleanup)

	cli, err := txnkv.NewClient(endpoints)
	if err != nil {
		t.Fatalf("dial PD %v: %v", endpoints, err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	// Probe with retry — the cluster may need a few seconds after container
	// readiness to elect a region leader and accept timestamps.
	probeCtx, probeCancel := context.WithTimeout(ctx, 30*time.Second)
	defer probeCancel()
	be := &tikvBackend{cli: cli}
	if err := waitProbe(probeCtx, be); err != nil {
		t.Fatalf("probe TiKV cluster at %v: %v", endpoints, err)
	}

	storetest.Run(t, func(t *testing.T) meta.Store {
		// Each subtest gets a Store on the shared backend handle. The
		// contract cases use unique bucket / object names so cross-case
		// state leakage is bounded.
		return openWithBackend(&tikvBackend{cli: cli})
	})
}

// acquireCluster returns PD endpoints reachable from the test host and a
// cleanup hook. Falls back through the priority list in TestTiKVStoreContract's
// docstring; either succeeds or t.Skipf's the test.
func acquireCluster(ctx context.Context, t *testing.T) ([]string, func()) {
	t.Helper()
	if env := os.Getenv("STRATA_TIKV_TEST_PD_ENDPOINTS"); env != "" {
		return strings.Split(env, ","), func() {}
	}
	endpoints, cleanup, err := setUpTiKVCluster(ctx, t)
	if err != nil {
		t.Skipf("set up testcontainers PD/TiKV: %v — set STRATA_TIKV_TEST_PD_ENDPOINTS to point at an operator-provided cluster, or fix the Docker setup", err)
	}
	return endpoints, cleanup
}

// setUpTiKVCluster spawns pingcap/pd + pingcap/tikv on a private docker
// network. Both services advertise their listen addresses as
// host.docker.internal:<host-port>, which:
//
//   - resolves inside containers (Docker Desktop / Lima inject the alias for
//     host-gateway; Linux CI gets it via the explicit ExtraHosts entry below)
//   - resolves on the test host (Docker Desktop / Lima publish it as a host
//     alias for the engine; on Linux CI the test process runs in the same
//     network namespace as Docker so the host-published port is reachable
//     via 127.0.0.1, and host.docker.internal in /etc/hosts on the runner)
//
// PD's intra-cluster URLs use the docker-network alias `pd`; the only
// addresses that need to round-trip via the gateway are PD's client URL and
// TiKV's RPC addr — both reachable from PD-the-container AND the test client.
//
// testcontainers-go panics (rather than returns) when DOCKER_HOST is unset on
// macOS+Lima setups; the deferred recover below converts that into a clean
// error so the caller can t.Skipf instead of crashing the test binary.
func setUpTiKVCluster(ctx context.Context, t *testing.T) (endpoints []string, cleanup func(), err error) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			endpoints = nil
			cleanup = nil
			err = fmt.Errorf("testcontainers panic: %v", r)
		}
	}()

	pdPort, err := freeTCPPort()
	if err != nil {
		return nil, nil, fmt.Errorf("free pd port: %w", err)
	}
	tikvPort, err := freeTCPPort()
	if err != nil {
		return nil, nil, fmt.Errorf("free tikv port: %w", err)
	}

	net, err := tcnetwork.New(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("create docker network: %w", err)
	}
	netCleanup := func() { _ = net.Remove(ctx) }

	addHostGateway := func(hc *container.HostConfig) {
		hc.ExtraHosts = append(hc.ExtraHosts, "host.docker.internal:host-gateway")
	}

	pdReq := testcontainers.ContainerRequest{
		Image:    "pingcap/pd:v8.5.0",
		Networks: []string{net.Name},
		NetworkAliases: map[string][]string{
			net.Name: {"pd"},
		},
		ExposedPorts: []string{fmt.Sprintf("%d:%d/tcp", pdPort, pdPort)},
		Cmd: []string{
			"--name=pd",
			fmt.Sprintf("--client-urls=http://0.0.0.0:%d", pdPort),
			fmt.Sprintf("--advertise-client-urls=http://host.docker.internal:%d", pdPort),
			"--peer-urls=http://0.0.0.0:2380",
			"--advertise-peer-urls=http://pd:2380",
			"--initial-cluster=pd=http://pd:2380",
			"--data-dir=/data",
		},
		HostConfigModifier: addHostGateway,
		WaitingFor: wait.ForHTTP("/pd/api/v1/health").
			WithPort(nat.Port(fmt.Sprintf("%d/tcp", pdPort))).
			WithStartupTimeout(60 * time.Second),
	}
	pdContainer, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: pdReq,
		Started:          true,
	})
	if err != nil {
		netCleanup()
		return nil, nil, fmt.Errorf("start PD: %w", err)
	}
	pdCleanup := func() { _ = pdContainer.Terminate(ctx) }

	tikvReq := testcontainers.ContainerRequest{
		Image:    "pingcap/tikv:v8.5.0",
		Networks: []string{net.Name},
		NetworkAliases: map[string][]string{
			net.Name: {"tikv"},
		},
		ExposedPorts: []string{fmt.Sprintf("%d:%d/tcp", tikvPort, tikvPort)},
		Cmd: []string{
			fmt.Sprintf("--pd=host.docker.internal:%d", pdPort),
			fmt.Sprintf("--addr=0.0.0.0:%d", tikvPort),
			fmt.Sprintf("--advertise-addr=host.docker.internal:%d", tikvPort),
			"--data-dir=/data",
		},
		HostConfigModifier: addHostGateway,
		WaitingFor: wait.ForLog("[INFO] [server.rs:").
			WithStartupTimeout(120 * time.Second),
	}
	tikvContainer, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: tikvReq,
		Started:          true,
	})
	if err != nil {
		pdCleanup()
		netCleanup()
		return nil, nil, fmt.Errorf("start TiKV: %w", err)
	}
	tikvCleanup := func() { _ = tikvContainer.Terminate(ctx) }

	cleanup = func() {
		tikvCleanup()
		pdCleanup()
		netCleanup()
	}
	return []string{fmt.Sprintf("host.docker.internal:%d", pdPort)}, cleanup, nil
}

func waitProbe(ctx context.Context, b *tikvBackend) error {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(30 * time.Second)
	}
	var lastErr error
	for time.Now().Before(deadline) {
		if err := b.Probe(ctx); err == nil {
			return nil
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if lastErr != nil {
		return lastErr
	}
	return ctx.Err()
}

func freeTCPPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	addr := l.Addr().(*net.TCPAddr)
	return addr.Port, nil
}
