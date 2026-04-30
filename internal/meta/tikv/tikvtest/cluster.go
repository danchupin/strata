//go:build integration

// Package tikvtest provides shared test-helper bring-up for a real
// PD + TiKV pair driven by testcontainers-go (or pointed at an
// operator-provided cluster via STRATA_TIKV_TEST_PD_ENDPOINTS).
//
// Two callers consume this today:
//
//   - internal/meta/tikv/store_integration_test.go — runs the meta.Store
//     contract suite against a real cluster.
//   - internal/s3api/race_integration_test.go — runs the gateway race
//     scenario (TestRaceMixedOpsTiKV) against a TiKV-backed fixture.
//
// Both build-tag-gate to integration. Sharing the bring-up keeps CI-vs-local
// behaviour and the host.docker.internal advertise pattern in lockstep.
package tikvtest

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
)

// AcquireCluster returns PD endpoints reachable from the test host and a
// cleanup hook. Cluster sourcing (in priority order):
//
//  1. STRATA_TIKV_TEST_PD_ENDPOINTS — comma-separated PD client addresses
//     for an operator-supplied cluster (CI workflows / docker-compose
//     stacks already bring this up); cleanup is a no-op.
//  2. Otherwise testcontainers-go spawns pingcap/pd + pingcap/tikv on a
//     private docker network with the host-gateway advertise pattern;
//     cleanup tears the containers + network down.
//
// On failure the helper t.Skipf's with the underlying error so a sandboxed
// runner without container support still marks the suite passing —
// surface contract is already validated against the in-process memory
// backend (parity oracle).
func AcquireCluster(ctx context.Context, t testing.TB) ([]string, func()) {
	t.Helper()
	if env := os.Getenv("STRATA_TIKV_TEST_PD_ENDPOINTS"); env != "" {
		parts := strings.Split(env, ",")
		for i, p := range parts {
			parts[i] = strings.TrimSpace(p)
		}
		return parts, func() {}
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
func setUpTiKVCluster(ctx context.Context, t testing.TB) (endpoints []string, cleanup func(), err error) {
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

func freeTCPPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	addr := l.Addr().(*net.TCPAddr)
	return addr.Port, nil
}

// WaitProbe polls store.Probe(ctx) until it succeeds or ctx deadline is hit.
// A real cluster needs a few seconds after container readiness to elect a
// region leader and accept timestamps, so callers should give a 30s+ ctx.
//
// The Probe accepts an interface (any type with `Probe(ctx) error`) so this
// helper can probe either *tikv.Store or any other prober without pulling
// the tikv package into a test-helper import cycle.
func WaitProbe(ctx context.Context, p interface{ Probe(context.Context) error }) error {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(30 * time.Second)
	}
	var lastErr error
	for time.Now().Before(deadline) {
		if err := p.Probe(ctx); err == nil {
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
