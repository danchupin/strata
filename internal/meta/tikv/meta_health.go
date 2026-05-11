package tikv

import (
	"context"
	"fmt"

	"github.com/danchupin/strata/internal/meta"
)

// MetaHealth returns a snapshot of TiKV/PD topology by hitting the PD
// /pd/api/v1/stores endpoint via the bootstrap-only pdClient. Raft-leader
// imbalance — any non-tombstone store reporting 0 leaders while peers have
// >0 — is folded into Warnings.
func (s *Store) MetaHealth(ctx context.Context) (report *meta.MetaHealthReport, err error) {
	ctx, finish := s.observer.Start(ctx, "MetaHealth", "meta_health")
	defer func() { finish(err) }()
	if len(s.cfg.PDEndpoints) == 0 {
		return &meta.MetaHealthReport{
			Backend:  "tikv",
			Warnings: []string{"no PD endpoints configured"},
		}, nil
	}

	client := newPDClient(s.cfg.PDEndpoints)
	var resp *pdStoresResponse
	resp, err = client.listStores(ctx)
	if err != nil {
		return nil, err
	}

	nodes := make([]meta.NodeStatus, 0, len(resp.Stores))
	var (
		anyHasLeaders   bool
		zeroLeaderStore []string
	)
	for _, st := range resp.Stores {
		dc, rack := labelsToDCRack(st.Store.Labels)
		nodes = append(nodes, meta.NodeStatus{
			Address:       st.Store.Address,
			State:         st.Store.StateName,
			SchemaVersion: st.Store.Version,
			DataCenter:    dc,
			Rack:          rack,
		})
		if isLiveStore(st.Store.StateName) {
			if st.Status.LeaderCount > 0 {
				anyHasLeaders = true
			} else {
				zeroLeaderStore = append(zeroLeaderStore, st.Store.Address)
			}
		}
	}

	var warnings []string
	if anyHasLeaders && len(zeroLeaderStore) > 0 {
		warnings = append(warnings, fmt.Sprintf(
			"raft-leader imbalance: %d stores have 0 leaders while peers carry traffic (%v)",
			len(zeroLeaderStore), zeroLeaderStore,
		))
	}

	rf := 0
	if len(nodes) > 0 {
		rf = 3
	}

	return &meta.MetaHealthReport{
		Backend:           "tikv",
		Nodes:             nodes,
		ReplicationFactor: rf,
		Warnings:          warnings,
	}, nil
}

func labelsToDCRack(labels []pdStoreLabel) (dc, rack string) {
	for _, l := range labels {
		switch l.Key {
		case "zone", "dc", "data_center":
			dc = l.Value
		case "rack", "host":
			if rack == "" {
				rack = l.Value
			}
		}
	}
	return dc, rack
}

// isLiveStore returns true for store states that should carry raft leaders.
// PD store_state_name values: Up, Disconnected, Down, Offline, Tombstone.
func isLiveStore(state string) bool {
	switch state {
	case "Up":
		return true
	default:
		return false
	}
}

var _ meta.HealthProbe = (*Store)(nil)
