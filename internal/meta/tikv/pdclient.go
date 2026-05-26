// Package tikv pdclient.go — a deliberately thin HTTP client for the PD
// (Placement Driver) inspection endpoints surfaced on /admin/v1/storage/meta.
//
// This client is bootstrap-only: it tries the configured PD endpoints in
// order with a per-endpoint timeout and returns the first non-error
// response. The data path's PD failover is owned by tikv/client-go (which
// refreshes /pd/api/v1/members on schedule and rebalances regions across PDs)
// — duplicating that here would be churn for no benefit. Operator-facing
// reads tolerate a single PD outage by stepping to the next endpoint and
// surfacing 0 stores with a warning when every endpoint fails.
package tikv

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// pdEndpointTimeout is the per-PD HTTP timeout. PD's /pd/api/v1/stores is
// a metadata read served from the PD leader's cache; 2 s is generous.
const pdEndpointTimeout = 2 * time.Second

// pdStore is the per-store JSON shape PD returns under /pd/api/v1/stores.
// Only the fields the storage page renders are decoded — everything else is
// ignored on purpose so a future PD field bump doesn't fail decoding.
type pdStore struct {
	Store struct {
		ID            uint64            `json:"id"`
		Address       string            `json:"address"`
		StatusAddress string            `json:"status_address"`
		Version       string            `json:"version"`
		Labels        []pdStoreLabel    `json:"labels"`
		StateName     string            `json:"state_name"`
		Tags          map[string]string `json:"-"`
	} `json:"store"`
	Status pdStoreStatus `json:"status"`
}

type pdStoreLabel struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// pdStoreStatus mirrors the /pd/api/v1/stores `status` block. PD v8.5+ emits
// disk sizes as human-readable strings ("1.5GiB"), and uptime / *_ts as
// strings too — kept as string here since the storage health probe surfaces
// them as labels, not numeric metrics.
type pdStoreStatus struct {
	LeaderCount int    `json:"leader_count"`
	RegionCount int    `json:"region_count"`
	Available   string `json:"available"`
	Capacity    string `json:"capacity"`
}

type pdStoresResponse struct {
	Count  int       `json:"count"`
	Stores []pdStore `json:"stores"`
}

// pdClient issues HTTP GETs against a list of PD endpoints, trying each in
// order with pdEndpointTimeout and returning the first non-error response.
type pdClient struct {
	endpoints []string
	http      *http.Client
	// scheme is "http" when TLS is unconfigured, "https" when a TLS bundle
	// is wired via TLSConfig. The fetchStores helper consults this when an
	// endpoint omits scheme.
	scheme string
}

// newPDClientWithTLS builds a pdClient whose http.Client carries a
// *tls.Config when tlsCfg.HasAny() returns true. CAFile populates the root
// pool; CertFile + KeyFile install a client cert for mTLS; SkipVerify
// short-circuits hostname + chain verification on this control-plane
// transport.
//
// Returns a zero-cert client on TLSConfig parse error — callers fail at
// boot via newTiKVBackend before MetaHealth ever runs, so this path is
// defensive only.
func newPDClientWithTLS(endpoints []string, tlsCfg TLSConfig) *pdClient {
	scheme := "http"
	transport := http.DefaultTransport
	if tlsCfg.HasAny() {
		scheme = "https"
		tc := &tls.Config{InsecureSkipVerify: tlsCfg.SkipVerify}
		if tlsCfg.CAFile != "" {
			if pemBytes, err := os.ReadFile(tlsCfg.CAFile); err == nil {
				pool := x509.NewCertPool()
				if pool.AppendCertsFromPEM(pemBytes) {
					tc.RootCAs = pool
				}
			}
		}
		if tlsCfg.CertFile != "" && tlsCfg.KeyFile != "" {
			if pair, err := tls.LoadX509KeyPair(tlsCfg.CertFile, tlsCfg.KeyFile); err == nil {
				tc.Certificates = []tls.Certificate{pair}
			}
		}
		transport = &http.Transport{TLSClientConfig: tc}
	}
	return &pdClient{
		endpoints: endpoints,
		http: &http.Client{
			Timeout:   pdEndpointTimeout,
			Transport: transport,
		},
		scheme: scheme,
	}
}

// listStores fetches /pd/api/v1/stores, trying each endpoint in turn.
// Returns the parsed response from the first endpoint that responds with
// HTTP 200.
func (c *pdClient) listStores(ctx context.Context) (*pdStoresResponse, error) {
	if len(c.endpoints) == 0 {
		return nil, errors.New("no PD endpoints configured")
	}
	var lastErr error
	for _, endpoint := range c.endpoints {
		out, err := c.fetchStores(ctx, endpoint)
		if err == nil {
			return out, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func (c *pdClient) fetchStores(ctx context.Context, endpoint string) (*pdStoresResponse, error) {
	url := strings.TrimRight(endpoint, "/")
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		scheme := c.scheme
		if scheme == "" {
			scheme = "http"
		}
		url = scheme + "://" + url
	}
	url += "/pd/api/v1/stores"

	reqCtx, cancel := context.WithTimeout(ctx, pdEndpointTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("pd %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pd %s: status %d", endpoint, resp.StatusCode)
	}
	var out pdStoresResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("pd %s: decode: %w", endpoint, err)
	}
	return &out, nil
}
