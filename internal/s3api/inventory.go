package s3api

import (
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/danchupin/strata/internal/meta"
)

// InventoryConfiguration is the parsed shape of the XML body the client sends
// for PUT /<bucket>?inventory&id=<id>. Validation here is intentionally narrow
// — only the fields required to (a) round-trip GET and (b) drive the worker
// (target bucket, schedule, included versions). Optional fields are accepted
// without coercion so the original blob round-trips byte-for-byte.
type inventoryConfigurationXML struct {
	XMLName                xml.Name `xml:"InventoryConfiguration"`
	ID                     string   `xml:"Id"`
	IsEnabled              bool     `xml:"IsEnabled"`
	Destination            *struct {
		S3BucketDestination *struct {
			Bucket    string `xml:"Bucket"`
			Format    string `xml:"Format"`
			Prefix    string `xml:"Prefix,omitempty"`
			AccountID string `xml:"AccountId,omitempty"`
		} `xml:"S3BucketDestination"`
	} `xml:"Destination"`
	Schedule *struct {
		Frequency string `xml:"Frequency"`
	} `xml:"Schedule"`
	IncludedObjectVersions string `xml:"IncludedObjectVersions"`
	Filter                 *struct {
		Prefix string `xml:"Prefix,omitempty"`
	} `xml:"Filter,omitempty"`
	OptionalFields *struct {
		Field []string `xml:"Field"`
	} `xml:"OptionalFields,omitempty"`
}

// listInventoryConfigurationsResult is the response shape for
// GET /<bucket>?inventory (list, no id).
type listInventoryConfigurationsResult struct {
	XMLName               xml.Name                    `xml:"ListInventoryConfigurationsResult"`
	InventoryConfigurations []inventoryConfigurationXML `xml:"InventoryConfiguration"`
	IsTruncated           bool                        `xml:"IsTruncated"`
}

func (s *Server) handleBucketInventory(w http.ResponseWriter, r *http.Request, bucket string) {
	id := r.URL.Query().Get("id")
	switch r.Method {
	case http.MethodGet:
		if id == "" {
			s.listBucketInventory(w, r, bucket)
			return
		}
		s.getBucketInventory(w, r, bucket, id)
	case http.MethodPut:
		if id == "" {
			writeError(w, r, ErrInvalidArgument)
			return
		}
		s.putBucketInventory(w, r, bucket, id)
	case http.MethodDelete:
		if id == "" {
			writeError(w, r, ErrInvalidArgument)
			return
		}
		s.deleteBucketInventory(w, r, bucket, id)
	default:
		writeError(w, r, ErrNotImplemented)
	}
}

func (s *Server) putBucketInventory(w http.ResponseWriter, r *http.Request, bucket, id string) {
	b, err := s.Meta.GetBucket(r.Context(), bucket)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil || len(body) == 0 {
		writeError(w, r, ErrMalformedXML)
		return
	}
	if err := validateInventoryBlob(body, id); err != nil {
		if errors.Is(err, errInventoryIDMismatch) {
			writeError(w, r, ErrInvalidArgument)
			return
		}
		writeError(w, r, ErrMalformedXML)
		return
	}
	if err := s.Meta.SetBucketInventoryConfig(r.Context(), b.ID, id, body); err != nil {
		mapMetaErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// errInventoryIDMismatch is returned by validateInventoryBlob when the URL id
// does not equal the body's <Id> element. Adminapi distinguishes it from a
// schema-shape failure so it can return a precise error code.
var errInventoryIDMismatch = errors.New("inventory: id mismatch")

// validateInventoryBlob mirrors the inline validation putBucketInventory used
// to do — extracted so adminapi can preflight an admin-side blob without
// duplicating the rules. Returns errInventoryIDMismatch when expectedID is
// non-empty and disagrees with cfg.ID.
func validateInventoryBlob(body []byte, expectedID string) error {
	var cfg inventoryConfigurationXML
	if err := xml.Unmarshal(body, &cfg); err != nil {
		return err
	}
	if cfg.ID == "" {
		return errors.New("inventory: id is required")
	}
	if expectedID != "" && cfg.ID != expectedID {
		return errInventoryIDMismatch
	}
	if cfg.Destination == nil || cfg.Destination.S3BucketDestination == nil ||
		strings.TrimSpace(cfg.Destination.S3BucketDestination.Bucket) == "" ||
		strings.TrimSpace(cfg.Destination.S3BucketDestination.Format) == "" {
		return errors.New("inventory: destination bucket+format required")
	}
	if cfg.Schedule == nil || cfg.Schedule.Frequency == "" {
		return errors.New("inventory: schedule frequency required")
	}
	switch strings.ToLower(cfg.Schedule.Frequency) {
	case "daily", "hourly", "weekly":
	default:
		return errors.New("inventory: schedule frequency must be Daily|Hourly|Weekly")
	}
	switch cfg.IncludedObjectVersions {
	case "All", "Current":
	default:
		return errors.New("inventory: included_object_versions must be All|Current")
	}
	return nil
}

func (s *Server) getBucketInventory(w http.ResponseWriter, r *http.Request, bucket, id string) {
	b, err := s.Meta.GetBucket(r.Context(), bucket)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	blob, err := s.Meta.GetBucketInventoryConfig(r.Context(), b.ID, id)
	if err != nil {
		if errors.Is(err, meta.ErrNoSuchInventoryConfig) {
			writeError(w, r, ErrNoSuchInventoryConfig)
			return
		}
		mapMetaErr(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(blob)
}

func (s *Server) deleteBucketInventory(w http.ResponseWriter, r *http.Request, bucket, id string) {
	b, err := s.Meta.GetBucket(r.Context(), bucket)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	if _, err := s.Meta.GetBucketInventoryConfig(r.Context(), b.ID, id); err != nil {
		if errors.Is(err, meta.ErrNoSuchInventoryConfig) {
			writeError(w, r, ErrNoSuchInventoryConfig)
			return
		}
		mapMetaErr(w, r, err)
		return
	}
	if err := s.Meta.DeleteBucketInventoryConfig(r.Context(), b.ID, id); err != nil {
		mapMetaErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listBucketInventory(w http.ResponseWriter, r *http.Request, bucket string) {
	b, err := s.Meta.GetBucket(r.Context(), bucket)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	configs, err := s.Meta.ListBucketInventoryConfigs(r.Context(), b.ID)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	ids := make([]string, 0, len(configs))
	for id := range configs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	resp := listInventoryConfigurationsResult{}
	for _, id := range ids {
		var cfg inventoryConfigurationXML
		if err := xml.Unmarshal(configs[id], &cfg); err != nil {
			continue
		}
		resp.InventoryConfigurations = append(resp.InventoryConfigurations, cfg)
	}
	writeXML(w, http.StatusOK, resp)
}
