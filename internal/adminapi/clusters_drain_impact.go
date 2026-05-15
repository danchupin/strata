package adminapi

import (
	"context"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/data/placement"
	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/rebalance"
	"github.com/danchupin/strata/internal/s3api"
)

// drainImpactCacheTTL bounds the dedup window for the synchronous bucket
// walk that backs GET /admin/v1/clusters/{id}/drain-impact (US-003 drain-
// transparency). Two minutes shorter than the PRD's 5-minute target would
// shorten the dedup window without changing the contract — picked 5m to
// match the PRD exactly.
const drainImpactCacheTTL = 5 * time.Minute

// drainImpactDefaultLimit matches the bucket-references precedent so the UI
// can paginate via the same {limit, offset} affordance.
const (
	drainImpactDefaultLimit = 100
	drainImpactMaxLimit     = 1000
)

// SuggestedPolicy is one operator-facing remediation offered per bucket
// in the /drain-impact response (US-003). Label is the human-readable
// description the UI renders in the BulkPlacementFixDialog (US-005)
// dropdown; Policy is the {clusterID: weight} body the operator would
// PUT to /admin/v1/buckets/{name}/placement.
//
// PlacementModeOverride (US-003 effective-placement) carries an optional
// PUT-body `mode` value the UI sends alongside Policy. Empty string
// means the suggestion is mode-agnostic (leave bucket mode unchanged).
// Non-empty values are one of "weighted" | "strict"; "weighted" is the
// "Flip to weighted" shortcut that lets a strict-stuck bucket auto-
// resolve via cluster weights without policy edit, "strict" pairs with
// a replacement Placement that keeps the compliance pin.
type SuggestedPolicy struct {
	Label                 string         `json:"label"`
	Policy                map[string]int `json:"policy"`
	PlacementModeOverride string         `json:"placement_mode_override,omitempty"`
}

// BucketImpactEntry is one row in the by_bucket section of the
// /drain-impact response. CurrentPolicy is nil (JSON null) when the
// bucket has no Placement — the suggested policies switch into "set
// initial policy" mode in that case.
type BucketImpactEntry struct {
	Name              string            `json:"name"`
	CurrentPolicy     map[string]int    `json:"current_policy"`
	Category          string            `json:"category"`
	ChunkCount        int64             `json:"chunk_count"`
	BytesUsed         int64             `json:"bytes_used"`
	SuggestedPolicies []SuggestedPolicy `json:"suggested_policies"`
}

// ClusterDrainImpactResponse is the wire shape returned by
// GET /admin/v1/clusters/{id}/drain-impact (US-003 drain-transparency).
//
// Categorized chunk counters mirror the /drain-progress endpoint so the
// UI uses the same heuristics on both surfaces. ByBucket is the paged
// slice (default limit=100, max 1000); TotalBuckets is the row count
// BEFORE pagination so the modal can render "showing N of M". Sort
// within the slice: stuck_single_policy > stuck_no_policy > migratable;
// within category descending by chunk_count then ascending by name.
type ClusterDrainImpactResponse struct {
	ClusterID               string              `json:"cluster_id"`
	CurrentState            string              `json:"current_state"`
	MigratableChunks        int64               `json:"migratable_chunks"`
	StuckSinglePolicyChunks int64               `json:"stuck_single_policy_chunks"`
	StuckNoPolicyChunks     int64               `json:"stuck_no_policy_chunks"`
	TotalChunks             int64               `json:"total_chunks"`
	ByBucket                []BucketImpactEntry `json:"by_bucket"`
	TotalBuckets            int                 `json:"total_buckets"`
	NextOffset              *int                `json:"next_offset"`
	LastScanAt              string              `json:"last_scan_at"`
}

// handleClusterDrainImpact serves GET /admin/v1/clusters/{id}/drain-
// impact. Callable only when state ∈ {live, draining_readonly};
// returns 409 Conflict for state ∈ {evacuating, removed} so operators
// are funneled to the live /drain-progress endpoint instead.
//
// The scan walks every bucket via ListObjects + GetBucketPlacement,
// categorizing each chunk that sits on the target cluster. Results are
// cached for 5 minutes per cluster (drainImpactCacheTTL) so concurrent
// admin viewers + the modal's mode-flip refetch do not stampede the
// store.
func (s *Server) handleClusterDrainImpact(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "cluster id is required")
		return
	}
	if s.Meta == nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", "meta store not configured")
		return
	}
	if len(s.KnownClusters) > 0 {
		if _, ok := s.KnownClusters[id]; !ok {
			writeJSONError(w, http.StatusBadRequest, "UnknownCluster",
				"cluster id is not configured (check STRATA_RADOS_CLUSTERS / STRATA_S3_CLUSTERS)")
			return
		}
	}

	ctx := r.Context()
	owner := auth.FromContext(ctx).Owner
	s3api.SetAuditOverride(ctx, "admin:GetClusterDrainImpact", "cluster:"+id, "-", owner)

	q := r.URL.Query()
	limit := parseRange(q.Get("limit"), drainImpactDefaultLimit, 1, drainImpactMaxLimit)
	offset := parseRange(q.Get("offset"), 0, 0, 1<<30)

	rows, err := s.Meta.ListClusterStates(ctx)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	currentRow := rows[id]
	currentState := currentRow.State
	if currentState == "" {
		currentState = meta.ClusterStateLive
	}
	if currentState != meta.ClusterStateLive && currentState != meta.ClusterStateDrainingReadonly {
		writeJSON(w, http.StatusConflict, invalidTransitionResponse{
			Code: "InvalidTransition",
			Message: "drain-impact only available for state ∈ {live, draining_readonly}; " +
				"use /drain-progress for state=" + currentState,
			CurrentState:  currentState,
			RequestedMode: "",
		})
		return
	}

	cache := s.drainImpact()
	scan, ok := cache.get(id)
	if !ok {
		fresh, err := computeDrainImpact(ctx, s.Meta, id, rows, s.knownClusterIDs())
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
			return
		}
		cache.set(id, fresh)
		scan = fresh
	}

	resp := scan.response(id, currentState, limit, offset)
	writeJSON(w, http.StatusOK, resp)
}

// drainImpactScan is the computed-once aggregate retained in the per-
// cluster cache. response() shapes the wire payload out of it on every
// request so pagination cursors are derived without re-scanning.
type drainImpactScan struct {
	MigratableChunks        int64
	StuckSinglePolicyChunks int64
	StuckNoPolicyChunks     int64
	TotalChunks             int64
	ByBucket                []BucketImpactEntry
	ScannedAt               time.Time
}

func (s drainImpactScan) response(clusterID, currentState string, limit, offset int) ClusterDrainImpactResponse {
	out := ClusterDrainImpactResponse{
		ClusterID:               clusterID,
		CurrentState:            currentState,
		MigratableChunks:        s.MigratableChunks,
		StuckSinglePolicyChunks: s.StuckSinglePolicyChunks,
		StuckNoPolicyChunks:     s.StuckNoPolicyChunks,
		TotalChunks:             s.TotalChunks,
		ByBucket:                []BucketImpactEntry{},
		TotalBuckets:            len(s.ByBucket),
		LastScanAt:              s.ScannedAt.UTC().Format(time.RFC3339),
	}
	if offset < len(s.ByBucket) {
		end := min(offset+limit, len(s.ByBucket))
		out.ByBucket = append(out.ByBucket, s.ByBucket[offset:end]...)
		if end < len(s.ByBucket) {
			next := end
			out.NextOffset = &next
		}
	}
	return out
}

// computeDrainImpact walks every bucket and counts chunks that sit on
// the target cluster (clusterID), classifying each bucket via
// rebalance.ClassifyBucket so the verdict matches the rebalance worker's
// per-tick categorization. excludeForTargets folds every draining-for-
// write cluster (readonly + evacuating + legacy draining) so a bucket
// whose policy spreads across two draining clusters surfaces correctly
// as stuck_single_policy. The clusterID being previewed is forcibly
// added to that set when its current state is "live" — the operator
// is asking "what happens if I drain this?" so the classifier must
// treat it as draining for the purpose of remaining-target counting.
//
// Live cluster set (knownClusterIDs minus excludeForTargets minus
// clusterID itself) is threaded into suggestedPoliciesForBucket so
// every bucket gets per-cluster single-target suggestions even when
// the operator forgot to seed cluster_state rows.
func computeDrainImpact(ctx context.Context, m meta.Store, clusterID string, rows map[string]meta.ClusterStateRow, knownIDs []string) (drainImpactScan, error) {
	excludeForTargets := map[string]bool{clusterID: true}
	for id, row := range rows {
		if meta.IsDrainingForWrite(row.State) {
			excludeForTargets[id] = true
		}
	}
	liveClusters := liveClusterIDs(knownIDs, excludeForTargets)

	// US-003 effective-placement: synthesise a "preview" state map that
	// mirrors the rebalance worker's view if the operator triggered the
	// drain now. excludeForTargets clusters (the previewed cluster plus
	// any already-draining peers) get a non-live placeholder row so
	// placement.EffectivePolicy filters bucketPolicy against the same
	// live-state predicate the PUT path uses. Clusters that are not in
	// excludeForTargets keep their actual row (or stay absent →
	// absence==live).
	previewStates := make(map[string]meta.ClusterStateRow, len(rows)+len(excludeForTargets))
	for id, row := range rows {
		previewStates[id] = row
	}
	for id := range excludeForTargets {
		row, ok := previewStates[id]
		if !ok || row.State == meta.ClusterStateLive || row.State == "" {
			row.State = meta.ClusterStateDrainingReadonly
			previewStates[id] = row
		}
	}
	previewWeights := placement.DefaultPolicy(previewStates)

	buckets, err := m.ListBuckets(ctx, "")
	if err != nil {
		return drainImpactScan{}, err
	}

	out := drainImpactScan{ScannedAt: time.Now().UTC()}
	for _, b := range buckets {
		if ctx.Err() != nil {
			return drainImpactScan{}, ctx.Err()
		}
		policy, perr := m.GetBucketPlacement(ctx, b.Name)
		if perr != nil {
			// Bucket vanished between ListBuckets and GetBucketPlacement
			// (race with concurrent DeleteBucket) — skip.
			continue
		}
		effective := placement.EffectivePolicy(policy, b.PlacementMode, previewWeights, previewStates)
		category := rebalance.ClassifyBucket(policy, effective, b.PlacementMode)
		entry := BucketImpactEntry{
			Name:     b.Name,
			Category: category,
		}
		if len(policy) > 0 {
			entry.CurrentPolicy = cloneIntMap(policy)
		}
		chunks, bytes, scanErr := chunksOnClusterForBucket(ctx, m, b.ID, clusterID)
		if scanErr != nil {
			// Same race as above — log via response (skip).
			continue
		}
		if chunks == 0 {
			// Bucket has no chunks on the draining cluster — exclude from
			// the impact roll-up. This handles buckets that reference
			// other clusters only.
			continue
		}
		entry.ChunkCount = chunks
		entry.BytesUsed = bytes
		entry.SuggestedPolicies = suggestedPoliciesForBucket(entry.CurrentPolicy, b.PlacementMode, category, excludeForTargets, liveClusters)
		out.ByBucket = append(out.ByBucket, entry)
		switch category {
		case "migratable":
			out.MigratableChunks += chunks
		case "stuck_single_policy":
			out.StuckSinglePolicyChunks += chunks
		case "stuck_no_policy":
			out.StuckNoPolicyChunks += chunks
		}
	}
	out.TotalChunks = out.MigratableChunks + out.StuckSinglePolicyChunks + out.StuckNoPolicyChunks
	sortBucketImpactEntries(out.ByBucket)
	return out, nil
}

// chunksOnClusterForBucket walks every latest-version object in the
// bucket and sums the chunks + bytes that sit on clusterID. Pages via
// the existing ListOptions iteration shape used by the rebalance
// scanner.
func chunksOnClusterForBucket(ctx context.Context, m meta.Store, bucketID [16]byte, clusterID string) (int64, int64, error) {
	var chunks, bytes int64
	opts := meta.ListOptions{Limit: 1000}
	for {
		if ctx.Err() != nil {
			return 0, 0, ctx.Err()
		}
		res, err := m.ListObjects(ctx, bucketID, opts)
		if err != nil {
			return 0, 0, err
		}
		for _, o := range res.Objects {
			if o.Manifest == nil {
				continue
			}
			for _, c := range o.Manifest.Chunks {
				if c.Cluster == clusterID {
					chunks++
					bytes += c.Size
				}
			}
			if br := o.Manifest.BackendRef; br != nil && br.Cluster == clusterID {
				chunks++
				bytes += br.Size
			}
		}
		if !res.Truncated {
			return chunks, bytes, nil
		}
		opts.Marker = res.NextMarker
	}
}

// liveClusterIDs returns the known cluster ids minus everything in the
// excludeForTargets set, sorted. Determinism matters for suggested
// policies — the UI shows them in a deterministic order so the operator
// sees stable choices across refreshes.
func liveClusterIDs(known []string, exclude map[string]bool) []string {
	out := make([]string, 0, len(known))
	for _, id := range known {
		if exclude[id] {
			continue
		}
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// knownClusterIDs returns Server.KnownClusters as a sorted slice. nil
// → empty slice so callers do not need to nil-check.
func (s *Server) knownClusterIDs() []string {
	if len(s.KnownClusters) == 0 {
		return nil
	}
	out := make([]string, 0, len(s.KnownClusters))
	for id := range s.KnownClusters {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// suggestedPoliciesForBucket builds the list of remediation policies
// offered for one bucket. Flavours:
//
//   - "Flip to weighted" — strict-stuck only (US-003 effective-
//     placement). Re-PUT placement carrying the bucket's existing
//     policy + mode="weighted" so cluster.weights auto-fallback
//     resolves the stuck state without policy edits.
//   - "Uniform live" — one entry that spreads weight 1 across every
//     live cluster (draining keys forced to 0 when the bucket already
//     has a policy).
//   - "Single-target replacement" — one entry per live cluster.
//
// PlacementModeOverride is stamped onto the strict-flip suggestion so
// the UI knows to PUT mode=weighted alongside the policy. The other
// suggestions leave it empty (mode unchanged on save).
//
// With no live clusters left, the list is empty so the UI hides the
// "Fix" CTA — there is no remediation the operator can apply from
// here.
func suggestedPoliciesForBucket(current map[string]int, mode, category string, exclude map[string]bool, live []string) []SuggestedPolicy {
	if len(live) == 0 && (category != "stuck_single_policy" || meta.NormalizePlacementMode(mode) != meta.PlacementModeStrict) {
		return nil
	}
	out := make([]SuggestedPolicy, 0, len(live)+2)

	if category == "stuck_single_policy" && meta.NormalizePlacementMode(mode) == meta.PlacementModeStrict && len(current) > 0 {
		// Flip-to-weighted shortcut: keep the bucket's policy but flip
		// mode so cluster.weights auto-fallback unsticks the bucket.
		out = append(out, SuggestedPolicy{
			Label:                 "Flip to weighted (auto-fallback to cluster weights)",
			Policy:                cloneIntMap(current),
			PlacementModeOverride: meta.PlacementModeWeighted,
		})
	}

	if len(live) > 0 {
		uniform := map[string]int{}
		for _, id := range live {
			uniform[id] = 1
		}
		uniformLabel := "Add all live clusters (uniform)"
		if current == nil {
			uniformLabel = "Set initial policy: live clusters uniform"
		} else {
			for id, w := range current {
				if exclude[id] && w > 0 {
					uniform[id] = 0
				}
			}
		}
		out = append(out, SuggestedPolicy{Label: uniformLabel, Policy: uniform})

		strictStuck := category == "stuck_single_policy" && meta.NormalizePlacementMode(mode) == meta.PlacementModeStrict
		for _, id := range live {
			label := "Replace draining with " + id
			if current == nil {
				label = "Set initial policy: " + id
			}
			sp := SuggestedPolicy{
				Label:  label,
				Policy: map[string]int{id: 1},
			}
			if strictStuck {
				// Preserve the compliance pin: replacement Placement +
				// mode=strict keeps the bucket strict-flagged so the
				// operator's data-sovereignty intent is preserved.
				sp.PlacementModeOverride = meta.PlacementModeStrict
				sp.Label = "Replace with " + id + " (keep strict)"
			}
			out = append(out, sp)
		}
	}
	return out
}

// sortBucketImpactEntries sorts in place: stuck_single_policy first,
// then stuck_no_policy, then migratable; within each category by
// chunk_count desc then name asc.
func sortBucketImpactEntries(entries []BucketImpactEntry) {
	priority := func(cat string) int {
		switch cat {
		case "stuck_single_policy":
			return 0
		case "stuck_no_policy":
			return 1
		case "migratable":
			return 2
		default:
			return 3
		}
	}
	sort.SliceStable(entries, func(i, j int) bool {
		pi, pj := priority(entries[i].Category), priority(entries[j].Category)
		if pi != pj {
			return pi < pj
		}
		if entries[i].ChunkCount != entries[j].ChunkCount {
			return entries[i].ChunkCount > entries[j].ChunkCount
		}
		return entries[i].Name < entries[j].Name
	})
}

func cloneIntMap(in map[string]int) map[string]int {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]int, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// drainImpact returns the lazily-initialised per-cluster TTL cache. Same
// pattern as Server.hotBuckets.
func (s *Server) drainImpact() *drainImpactCache {
	s.drainImpactMu.Lock()
	defer s.drainImpactMu.Unlock()
	if s.drainImpactCacheVal == nil {
		s.drainImpactCacheVal = &drainImpactCache{ttl: drainImpactCacheTTL, now: time.Now}
	}
	return s.drainImpactCacheVal
}

// drainImpactCache is a per-cluster TTL cache for drainImpactScan. The
// keyspace is bounded by the configured cluster count, so no LRU
// eviction is needed.
type drainImpactCache struct {
	mu      sync.Mutex
	entries map[string]drainImpactCacheEntry
	ttl     time.Duration
	now     func() time.Time
}

type drainImpactCacheEntry struct {
	expires time.Time
	payload drainImpactScan
}

func (c *drainImpactCache) get(clusterID string) (drainImpactScan, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[clusterID]
	if !ok || c.now().After(e.expires) {
		return drainImpactScan{}, false
	}
	return e.payload, true
}

func (c *drainImpactCache) set(clusterID string, payload drainImpactScan) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]drainImpactCacheEntry)
	}
	c.entries[clusterID] = drainImpactCacheEntry{
		expires: c.now().Add(c.ttl),
		payload: payload,
	}
}

// InvalidateAll drops every cached drainImpactScan entry. Called
// synchronously by bucket-mutation handlers (PUT/DELETE
// /admin/v1/buckets/{name}/placement, DELETE
// /admin/v1/buckets/{name}) before returning 2xx so the next
// /drain-impact GET reflects the new policy immediately — without
// waiting out drainImpactCacheTTL. Invalidate-all over per-cluster
// diff: placement keys may add/remove clusters; tracking the
// affected set adds complexity for a minor speedup (cache miss only
// rescans clusters the operator is actively previewing).
func (c *drainImpactCache) InvalidateAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = nil
}

