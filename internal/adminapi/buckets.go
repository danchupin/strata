package adminapi

import "net/http"

// handleBucketsList serves GET /admin/v1/buckets. Phase 1 stub.
func (s *Server) handleBucketsList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, BucketsListResponse{
		Buckets: []BucketSummary{},
		Total:   0,
	})
}

// handleBucketsTop serves GET /admin/v1/buckets/top. Phase 1 stub for the
// home-page widget (US-007 fills it in).
func (s *Server) handleBucketsTop(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, BucketsTopResponse{Buckets: []BucketTop{}})
}

// handleBucketGet serves GET /admin/v1/buckets/{bucket}. Phase 1 stub —
// always 404 until US-011 wires the meta.Store lookup.
func (s *Server) handleBucketGet(w http.ResponseWriter, r *http.Request) {
	writeJSONError(w, http.StatusNotFound, "NoSuchBucket", "bucket not found")
}

// handleObjectsList serves GET /admin/v1/buckets/{bucket}/objects. Phase 1 stub.
func (s *Server) handleObjectsList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, ObjectsListResponse{
		Objects:        []ObjectSummary{},
		CommonPrefixes: []string{},
	})
}
