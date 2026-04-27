package cassandra

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/gocql/gocql"
	"go.opentelemetry.io/otel/trace"
)

type SessionConfig struct {
	Hosts       []string
	Keyspace    string
	LocalDC     string
	Replication string
	Username    string
	Password    string
	Timeout     time.Duration
	// Logger receives slow-query WARN lines when SlowMS > 0. Nil disables.
	Logger *slog.Logger
	// SlowMS controls the slow-query threshold in milliseconds.
	// 0 disables logging; defaults are loaded by callers via SlowMSFromEnv.
	SlowMS int
	// Metrics, when set, records every query into a metrics sink (latency
	// histogram by table+op). Nil disables; binaries plug in
	// metrics.CassandraObserver{}.
	Metrics Metrics
	// Tracer, when set, emits one OTel child span per query. Binaries plug
	// in tracerProvider.Tracer("strata.meta.cassandra"). Nil disables.
	Tracer trace.Tracer
}

func newCluster(cfg SessionConfig) *gocql.ClusterConfig {
	c := gocql.NewCluster(cfg.Hosts...)
	c.Consistency = gocql.LocalQuorum
	c.SerialConsistency = gocql.LocalSerial
	c.ProtoVersion = 4
	if cfg.Timeout > 0 {
		c.Timeout = cfg.Timeout
		c.ConnectTimeout = cfg.Timeout
	}
	if cfg.LocalDC != "" {
		c.PoolConfig.HostSelectionPolicy = gocql.TokenAwareHostPolicy(
			gocql.DCAwareRoundRobinPolicy(cfg.LocalDC),
		)
	}
	if cfg.Username != "" {
		c.Authenticator = gocql.PasswordAuthenticator{
			Username: cfg.Username,
			Password: cfg.Password,
		}
	}
	if obs := NewQueryObserver(cfg.Logger, time.Duration(cfg.SlowMS)*time.Millisecond, cfg.Metrics, cfg.Tracer); obs != nil {
		c.QueryObserver = obs
	}
	return c
}

func connect(cfg SessionConfig) (*gocql.Session, error) {
	c := newCluster(cfg)
	c.Keyspace = cfg.Keyspace
	s, err := c.CreateSession()
	if err != nil {
		return nil, fmt.Errorf("cassandra connect: %w", err)
	}
	return s, nil
}

func connectNoKeyspace(cfg SessionConfig) (*gocql.Session, error) {
	c := newCluster(cfg)
	s, err := c.CreateSession()
	if err != nil {
		return nil, fmt.Errorf("cassandra bootstrap connect: %w", err)
	}
	return s, nil
}
