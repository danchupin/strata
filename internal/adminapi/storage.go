package adminapi

import (
	"net/http"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/meta"
)

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
