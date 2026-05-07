package adminapi

import (
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"strings"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/meta"
)

// StorageClassEntry is one (class, bytes, objects) row in
// StorageClassesResponse. Sorted bytes-desc, class-asc tiebreak.
type StorageClassEntry struct {
	Class   string `json:"class"`
	Bytes   int64  `json:"bytes"`
	Objects int64  `json:"objects"`
}

// StorageClassesResponse is the JSON shape returned by GET
// /admin/v1/storage/classes. Classes carries the cluster-wide per-class
// totals computed by the bucketstats sampler; PoolsByClass mirrors the
// configured rados [classes] map (empty for memory / s3 backends).
type StorageClassesResponse struct {
	Classes      []StorageClassEntry `json:"classes"`
	PoolsByClass map[string]string   `json:"pools_by_class"`
}

// handleStorageMeta serves GET /admin/v1/storage/meta. Type-asserts the
// configured meta.Store against the optional meta.HealthProbe surface and
// returns the report as JSON. Backends that do not implement the probe
// (none today, but the interface is intentionally optional) get a 503 so
// the storage page can render an explainer instead of a generic 500.
func (s *Server) handleStorageMeta(w http.ResponseWriter, r *http.Request) {
	probe, ok := s.Meta.(meta.HealthProbe)
	if !ok {
		writeJSONError(w, http.StatusServiceUnavailable,
			"NotImplemented",
			"meta backend does not expose health probe")
		return
	}
	report, err := probe.MetaHealth(r.Context())
	if err != nil {
		s.Logger.Printf("adminapi: storage/meta: %v", err)
		writeJSONError(w, http.StatusBadGateway,
			"MetaHealthFailed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// handleStorageData serves GET /admin/v1/storage/data. Type-asserts the
// configured data.Backend against the optional data.HealthProbe surface and
// returns the report as JSON. Backends that do not implement the probe
// (currently no production backend; rados/s3/memory all do) get a 503 so
// the storage page can render an explainer instead of a generic 500.
func (s *Server) handleStorageData(w http.ResponseWriter, r *http.Request) {
	if s.Data == nil {
		writeJSONError(w, http.StatusServiceUnavailable,
			"NotImplemented",
			"data backend not configured")
		return
	}
	probe, ok := s.Data.(data.HealthProbe)
	if !ok {
		writeJSONError(w, http.StatusServiceUnavailable,
			"NotImplemented",
			"data backend does not expose health probe")
		return
	}
	report, err := probe.DataHealth(r.Context())
	if err != nil {
		s.Logger.Printf("adminapi: storage/data: %v", err)
		writeJSONError(w, http.StatusBadGateway,
			"DataHealthFailed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// handleStorageClasses serves GET /admin/v1/storage/classes. Returns the
// cluster-wide per-storage-class byte+object totals captured by the
// bucketstats sampler at its last pass plus the configured class -> pool
// map (empty for memory / s3-over-s3 backends). When no snapshot is wired
// (test rigs without a sampler) the handler still returns 200 with empty
// arrays so the UI can mount cleanly.
func (s *Server) handleStorageClasses(w http.ResponseWriter, r *http.Request) {
	totals := s.StorageClasses.Classes()
	pools := s.StorageClasses.Pools()
	classes := make([]StorageClassEntry, 0, len(totals))
	for class, st := range totals {
		classes = append(classes, StorageClassEntry{
			Class:   class,
			Bytes:   st.Bytes,
			Objects: st.Objects,
		})
	}
	sort.Slice(classes, func(i, j int) bool {
		if classes[i].Bytes != classes[j].Bytes {
			return classes[i].Bytes > classes[j].Bytes
		}
		return classes[i].Class < classes[j].Class
	})
	writeJSON(w, http.StatusOK, StorageClassesResponse{
		Classes:      classes,
		PoolsByClass: pools,
	})
}

// StorageHealthResponse is the JSON shape for GET /admin/v1/storage/health
// (US-005 cycle aggregate). ok=false when either the meta or the data probe
// reports Warnings.length > 0 OR any node/pool is in a non-OK state. Source
// names the first backend that flipped ok to false ("meta" wins ties); empty
// when ok=true.
type StorageHealthResponse struct {
	OK       bool     `json:"ok"`
	Warnings []string `json:"warnings"`
	Source   string   `json:"source,omitempty"`
}

// storageHealthOverrideEnv is the test-only knob that short-circuits the
// aggregate handler with a verbatim JSON payload — used by Playwright e2e to
// simulate degraded states without standing up a broken Cassandra cluster.
const storageHealthOverrideEnv = "STRATA_STORAGE_HEALTH_OVERRIDE"

// nodeStateOK lists per-node states that the aggregate considers healthy.
// Cassandra/memory emit "UP"; TiKV (via PD) emits "Up"; everything else
// (Down/Disconnected/Tombstone/Offline/...) is treated as degraded.
func nodeStateOK(state string) bool {
	switch strings.ToLower(state) {
	case "up":
		return true
	}
	return false
}

// poolStateOK lists per-pool states the aggregate considers healthy.
// rados emits "ok"; memory + s3 emit "reachable"; anything else (notably
// "error") flips the aggregate.
func poolStateOK(state string) bool {
	switch strings.ToLower(state) {
	case "ok", "reachable":
		return true
	}
	return false
}

// handleStorageHealth serves GET /admin/v1/storage/health. Combines the meta
// + data probes into one boolean + worst-of warnings. Backs the
// <StorageDegradedBanner> on every page so the operator notices problems
// without navigating to /storage. Honors the STRATA_STORAGE_HEALTH_OVERRIDE
// env-var when set so e2e specs can simulate a degraded cluster.
func (s *Server) handleStorageHealth(w http.ResponseWriter, r *http.Request) {
	if raw := os.Getenv(storageHealthOverrideEnv); raw != "" {
		w.Header().Set("Content-Type", "application/json")
		// Validate the override is well-formed JSON; otherwise fall through
		// to the live computation so a misconfigured env-var doesn't blank
		// the banner permanently.
		var probe map[string]any
		if err := json.Unmarshal([]byte(raw), &probe); err == nil {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(raw))
			return
		}
		s.Logger.Printf("adminapi: %s is not valid JSON; ignoring", storageHealthOverrideEnv)
	}

	resp := StorageHealthResponse{OK: true, Warnings: []string{}}
	ctx := r.Context()

	if probe, ok := s.Meta.(meta.HealthProbe); ok {
		if rep, err := probe.MetaHealth(ctx); err == nil && rep != nil {
			degraded := len(rep.Warnings) > 0
			for _, n := range rep.Nodes {
				if !nodeStateOK(n.State) {
					degraded = true
					resp.Warnings = append(resp.Warnings,
						"meta node "+n.Address+" state="+n.State)
				}
			}
			resp.Warnings = append(resp.Warnings, rep.Warnings...)
			if degraded {
				resp.OK = false
				resp.Source = "meta"
			}
		} else if err != nil {
			resp.OK = false
			resp.Source = "meta"
			resp.Warnings = append(resp.Warnings, "meta probe error: "+err.Error())
		}
	}

	if s.Data != nil {
		if probe, ok := s.Data.(data.HealthProbe); ok {
			if rep, err := probe.DataHealth(ctx); err == nil && rep != nil {
				degraded := len(rep.Warnings) > 0
				for _, p := range rep.Pools {
					if !poolStateOK(p.State) {
						degraded = true
						resp.Warnings = append(resp.Warnings,
							"data pool "+p.Name+" state="+p.State)
					}
				}
				resp.Warnings = append(resp.Warnings, rep.Warnings...)
				if degraded {
					resp.OK = false
					if resp.Source == "" {
						resp.Source = "data"
					}
				}
			} else if err != nil {
				resp.OK = false
				if resp.Source == "" {
					resp.Source = "data"
				}
				resp.Warnings = append(resp.Warnings, "data probe error: "+err.Error())
			}
		}
	}

	writeJSON(w, http.StatusOK, resp)
}
