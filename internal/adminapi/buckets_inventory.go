package adminapi

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/s3api"
)

// InventoryConfigJSON is the operator-console wire shape for one bucket
// Inventory configuration. Mirrors the AWS InventoryConfiguration XML body
// the s3api consumer reads — admin endpoint translates between this JSON
// shape and the stored XML blob.
type InventoryConfigJSON struct {
	ID                     string                `json:"id"`
	IsEnabled              bool                  `json:"is_enabled"`
	Destination            InventoryDestJSON     `json:"destination"`
	Schedule               InventoryScheduleJSON `json:"schedule"`
	IncludedObjectVersions string                `json:"included_object_versions"`
	Filter                 *InventoryFilterJSON  `json:"filter,omitempty"`
	OptionalFields         []string              `json:"optional_fields,omitempty"`
}

type InventoryDestJSON struct {
	Bucket    string `json:"bucket"`
	Format    string `json:"format"`
	Prefix    string `json:"prefix,omitempty"`
	AccountID string `json:"account_id,omitempty"`
}

type InventoryScheduleJSON struct {
	Frequency string `json:"frequency"`
}

type InventoryFilterJSON struct {
	Prefix string `json:"prefix,omitempty"`
}

// InventoryConfigsListJSON is the list-response shape.
type InventoryConfigsListJSON struct {
	Configurations []InventoryConfigJSON `json:"configurations"`
}

// validInventoryFormats / validInventoryFrequencies / validInventoryVersions
// mirror the s3api consumer accept-set so we can reject typos before the
// XML round-trip.
var validInventoryFormats = map[string]struct{}{
	"CSV": {}, "ORC": {}, "Parquet": {},
}

var validInventoryFrequencies = map[string]struct{}{
	"Daily": {}, "Hourly": {}, "Weekly": {},
}

var validInventoryVersions = map[string]struct{}{
	"All": {}, "Current": {},
}

// inventoryConfigXML is the AWS InventoryConfiguration XML wire shape — a
// duplicate of s3api's unexported struct, kept here so the admin handler can
// own its JSON↔XML translation without leaking the s3api internals.
type inventoryConfigXML struct {
	XMLName     xml.Name              `xml:"InventoryConfiguration"`
	ID          string                `xml:"Id"`
	IsEnabled   bool                  `xml:"IsEnabled"`
	Destination *inventoryDestXML     `xml:"Destination,omitempty"`
	Schedule    *inventoryScheduleXML `xml:"Schedule,omitempty"`
	IncludedObjectVersions string `xml:"IncludedObjectVersions"`
	Filter         *inventoryFilterXML `xml:"Filter,omitempty"`
	OptionalFields *inventoryFieldsXML `xml:"OptionalFields,omitempty"`
}

type inventoryDestXML struct {
	S3BucketDestination *inventoryS3DestXML `xml:"S3BucketDestination,omitempty"`
}

type inventoryS3DestXML struct {
	Bucket    string `xml:"Bucket"`
	Format    string `xml:"Format"`
	Prefix    string `xml:"Prefix,omitempty"`
	AccountID string `xml:"AccountId,omitempty"`
}

type inventoryScheduleXML struct {
	Frequency string `xml:"Frequency"`
}

type inventoryFilterXML struct {
	Prefix string `xml:"Prefix,omitempty"`
}

type inventoryFieldsXML struct {
	Field []string `xml:"Field"`
}

func encodeInventoryXML(cfg *InventoryConfigJSON) ([]byte, error) {
	out := inventoryConfigXML{
		ID:        cfg.ID,
		IsEnabled: cfg.IsEnabled,
		Destination: &inventoryDestXML{
			S3BucketDestination: &inventoryS3DestXML{
				Bucket:    cfg.Destination.Bucket,
				Format:    cfg.Destination.Format,
				Prefix:    cfg.Destination.Prefix,
				AccountID: cfg.Destination.AccountID,
			},
		},
		Schedule:               &inventoryScheduleXML{Frequency: cfg.Schedule.Frequency},
		IncludedObjectVersions: cfg.IncludedObjectVersions,
	}
	if cfg.Filter != nil && strings.TrimSpace(cfg.Filter.Prefix) != "" {
		out.Filter = &inventoryFilterXML{Prefix: cfg.Filter.Prefix}
	}
	if len(cfg.OptionalFields) > 0 {
		out.OptionalFields = &inventoryFieldsXML{Field: append([]string(nil), cfg.OptionalFields...)}
	}
	return xml.Marshal(out)
}

func decodeInventoryXML(blob []byte) (*InventoryConfigJSON, error) {
	var x inventoryConfigXML
	if err := xml.Unmarshal(blob, &x); err != nil {
		return nil, err
	}
	out := &InventoryConfigJSON{
		ID:                     x.ID,
		IsEnabled:              x.IsEnabled,
		IncludedObjectVersions: x.IncludedObjectVersions,
	}
	if x.Destination != nil && x.Destination.S3BucketDestination != nil {
		out.Destination = InventoryDestJSON{
			Bucket:    x.Destination.S3BucketDestination.Bucket,
			Format:    x.Destination.S3BucketDestination.Format,
			Prefix:    x.Destination.S3BucketDestination.Prefix,
			AccountID: x.Destination.S3BucketDestination.AccountID,
		}
	}
	if x.Schedule != nil {
		out.Schedule = InventoryScheduleJSON{Frequency: x.Schedule.Frequency}
	}
	if x.Filter != nil && x.Filter.Prefix != "" {
		out.Filter = &InventoryFilterJSON{Prefix: x.Filter.Prefix}
	}
	if x.OptionalFields != nil && len(x.OptionalFields.Field) > 0 {
		out.OptionalFields = append([]string(nil), x.OptionalFields.Field...)
	}
	return out, nil
}

func validateInventoryConfig(cfg *InventoryConfigJSON, expectedID string) error {
	if strings.TrimSpace(cfg.ID) == "" {
		return errors.New("id is required")
	}
	if expectedID != "" && cfg.ID != expectedID {
		return fmt.Errorf("id %q does not match URL configID %q", cfg.ID, expectedID)
	}
	if strings.TrimSpace(cfg.Destination.Bucket) == "" {
		return errors.New("destination.bucket is required")
	}
	if _, ok := validInventoryFormats[cfg.Destination.Format]; !ok {
		return fmt.Errorf("destination.format %q must be CSV|ORC|Parquet", cfg.Destination.Format)
	}
	if _, ok := validInventoryFrequencies[cfg.Schedule.Frequency]; !ok {
		return fmt.Errorf("schedule.frequency %q must be Daily|Hourly|Weekly", cfg.Schedule.Frequency)
	}
	if _, ok := validInventoryVersions[cfg.IncludedObjectVersions]; !ok {
		return fmt.Errorf("included_object_versions %q must be All|Current", cfg.IncludedObjectVersions)
	}
	return nil
}

// handleBucketListInventory serves GET /admin/v1/buckets/{bucket}/inventory.
// Returns 200 + {configurations:[…]} sorted by ID. Empty list is a valid
// response (not 404) so the UI can render an "Add inventory" empty state.
func (s *Server) handleBucketListInventory(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("bucket")
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "bucket name is required")
		return
	}
	if s.Meta == nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", "meta store not configured")
		return
	}
	b, err := s.Meta.GetBucket(r.Context(), name)
	if err != nil {
		if errors.Is(err, meta.ErrBucketNotFound) {
			writeJSONError(w, http.StatusNotFound, "NoSuchBucket", "bucket not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	configs, err := s.Meta.ListBucketInventoryConfigs(r.Context(), b.ID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	ids := make([]string, 0, len(configs))
	for id := range configs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := InventoryConfigsListJSON{Configurations: make([]InventoryConfigJSON, 0, len(ids))}
	for _, id := range ids {
		cfg, derr := decodeInventoryXML(configs[id])
		if derr != nil {
			continue
		}
		out.Configurations = append(out.Configurations, *cfg)
	}
	writeJSON(w, http.StatusOK, out)
}

// handleBucketSetInventory serves PUT /admin/v1/buckets/{bucket}/inventory/{configID}.
// Body is InventoryConfigJSON; admin layer validates, encodes XML, runs the
// s3api preflight, persists. Audit override admin:SetInventoryConfig.
func (s *Server) handleBucketSetInventory(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	name := r.PathValue("bucket")
	configID := r.PathValue("configID")
	if name == "" || configID == "" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest",
			"bucket name and configID are required")
		return
	}
	if s.Meta == nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", "meta store not configured")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "failed to read body")
		return
	}
	var req InventoryConfigJSON
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "malformed JSON body")
		return
	}
	// If the body omitted ID, fall back to the URL configID for ergonomics.
	if strings.TrimSpace(req.ID) == "" {
		req.ID = configID
	}
	if vErr := validateInventoryConfig(&req, configID); vErr != nil {
		writeJSONError(w, http.StatusBadRequest, "InvalidArgument", vErr.Error())
		return
	}
	xmlBlob, eerr := encodeInventoryXML(&req)
	if eerr != nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", eerr.Error())
		return
	}
	if vErr := s3api.ValidateInventoryBlob(xmlBlob, configID); vErr != nil {
		writeJSONError(w, http.StatusBadRequest, "InvalidArgument", vErr.Error())
		return
	}

	ctx := r.Context()
	owner := auth.FromContext(ctx).Owner
	s3api.SetAuditOverride(ctx, "admin:SetInventoryConfig",
		"bucket-inventory:"+name+"/"+configID, name, owner)

	b, err := s.Meta.GetBucket(ctx, name)
	if err != nil {
		if errors.Is(err, meta.ErrBucketNotFound) {
			writeJSONError(w, http.StatusNotFound, "NoSuchBucket", "bucket not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	if err := s.Meta.SetBucketInventoryConfig(ctx, b.ID, configID, xmlBlob); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, req)
}

// handleBucketDeleteInventory serves DELETE /admin/v1/buckets/{bucket}/inventory/{configID}.
// Idempotent: missing config returns 204 (avoids a pre-check round-trip).
// Audit override admin:DeleteInventoryConfig.
func (s *Server) handleBucketDeleteInventory(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("bucket")
	configID := r.PathValue("configID")
	if name == "" || configID == "" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest",
			"bucket name and configID are required")
		return
	}
	if s.Meta == nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", "meta store not configured")
		return
	}
	ctx := r.Context()
	owner := auth.FromContext(ctx).Owner
	s3api.SetAuditOverride(ctx, "admin:DeleteInventoryConfig",
		"bucket-inventory:"+name+"/"+configID, name, owner)
	b, err := s.Meta.GetBucket(ctx, name)
	if err != nil {
		if errors.Is(err, meta.ErrBucketNotFound) {
			writeJSONError(w, http.StatusNotFound, "NoSuchBucket", "bucket not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	if err := s.Meta.DeleteBucketInventoryConfig(ctx, b.ID, configID); err != nil {
		if errors.Is(err, meta.ErrNoSuchInventoryConfig) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
