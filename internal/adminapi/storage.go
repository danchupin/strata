package adminapi

import (
	"net/http"
	"sort"

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
