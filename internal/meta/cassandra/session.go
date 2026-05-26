package cassandra

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"os"
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
	// TLS wires gocql.SslOptions when any field is set (US-004
	// harden-gateway). Empty CAFile + CertFile + KeyFile = plain-TCP =
	// current backwards-compat behavior. SkipVerify=true logs a WARN at
	// boot via the caller (serverapp).
	TLS TLSConfig
}

// TLSConfig is the subset of CassandraTLSConfig consumed by the gocql cluster
// builder. The serverapp layer translates internal/config.CassandraTLSConfig
// → cassandra.TLSConfig at the boundary so this package never imports
// internal/config.
type TLSConfig struct {
	CAFile     string
	CertFile   string
	KeyFile    string
	SkipVerify bool
}

// HasAny reports whether any TLS knob is set. Used by the cluster builder to
// decide between plain-TCP (zero value) and SslOpts-wired TLS.
func (t TLSConfig) HasAny() bool {
	return t.CAFile != "" || t.CertFile != "" || t.KeyFile != "" || t.SkipVerify
}

func newCluster(cfg SessionConfig) (*gocql.ClusterConfig, error) {
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
	if cfg.TLS.HasAny() {
		ssl, err := buildSslOptions(cfg.TLS)
		if err != nil {
			return nil, err
		}
		c.SslOpts = ssl
	}
	return c, nil
}

// buildSslOptions translates the SessionConfig TLS subset into a
// gocql.SslOptions value. Behavior:
//
//   - CAFile (PEM) populates tls.Config.RootCAs. Empty + SkipVerify=false →
//     the system root pool is used (Go's tls default).
//   - CertFile + KeyFile (PEM) populate tls.Config.Certificates for mTLS.
//   - SkipVerify=true sets InsecureSkipVerify and leaves
//     EnableHostVerification=false; otherwise EnableHostVerification=true so
//     the host SAN/CN is checked against the cassandra contact points.
func buildSslOptions(tcfg TLSConfig) (*gocql.SslOptions, error) {
	tc := &tls.Config{}
	if tcfg.CAFile != "" {
		pem, err := os.ReadFile(tcfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("cassandra tls ca_file %s: %w", tcfg.CAFile, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("cassandra tls ca_file %s: no certificates parsed", tcfg.CAFile)
		}
		tc.RootCAs = pool
	}
	if tcfg.CertFile != "" && tcfg.KeyFile != "" {
		pair, err := tls.LoadX509KeyPair(tcfg.CertFile, tcfg.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("cassandra tls cert/key: %w", err)
		}
		tc.Certificates = []tls.Certificate{pair}
	}
	tc.InsecureSkipVerify = tcfg.SkipVerify
	return &gocql.SslOptions{
		Config:                 tc,
		EnableHostVerification: !tcfg.SkipVerify,
	}, nil
}

func connect(cfg SessionConfig) (*gocql.Session, error) {
	c, err := newCluster(cfg)
	if err != nil {
		return nil, fmt.Errorf("cassandra connect: %w", err)
	}
	c.Keyspace = cfg.Keyspace
	s, err := c.CreateSession()
	if err != nil {
		return nil, fmt.Errorf("cassandra connect: %w", err)
	}
	return s, nil
}

func connectNoKeyspace(cfg SessionConfig) (*gocql.Session, error) {
	c, err := newCluster(cfg)
	if err != nil {
		return nil, fmt.Errorf("cassandra bootstrap connect: %w", err)
	}
	s, err := c.CreateSession()
	if err != nil {
		return nil, fmt.Errorf("cassandra bootstrap connect: %w", err)
	}
	return s, nil
}
