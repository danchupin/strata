package cassandra

import (
	"fmt"
	"time"

	"github.com/gocql/gocql"
)

type SessionConfig struct {
	Hosts       []string
	Keyspace    string
	LocalDC     string
	Replication string
	Username    string
	Password    string
	Timeout     time.Duration
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
