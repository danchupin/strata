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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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

type pdStoreStatus struct {
	LeaderCount int   `json:"leader_count"`
	RegionCount int   `json:"region_count"`
	Available   int64 `json:"available"`
	Capacity    int64 `json:"capacity"`
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
}

func newPDClient(endpoints []string) *pdClient {
	return &pdClient{
		endpoints: endpoints,
		http: &http.Client{
			Timeout: pdEndpointTimeout,
		},
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
		url = "http://" + url
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
