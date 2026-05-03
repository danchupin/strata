package adminapi

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/lifecycle"
	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/s3api"
)

// handleBucketGetLifecycle serves GET /admin/v1/buckets/{bucket}/lifecycle
// (US-004). Returns 200 + LifecycleConfigJSON, 404 NoSuchLifecycleConfiguration
// when no rules are stored, 404 NoSuchBucket when the bucket itself is missing.
//
// The stored blob is XML (the s3api consumer expects that shape on disk); the
// admin endpoint translates to AWS-shape JSON for the operator console.
func (s *Server) handleBucketGetLifecycle(w http.ResponseWriter, r *http.Request) {
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
	blob, err := s.Meta.GetBucketLifecycle(r.Context(), b.ID)
	if err != nil {
		if errors.Is(err, meta.ErrNoSuchLifecycle) {
			writeJSONError(w, http.StatusNotFound, "NoSuchLifecycleConfiguration",
				"no lifecycle configuration on bucket")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	cfg, derr := decodeLifecycleXML(blob)
	if derr != nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal",
			"stored lifecycle blob is not valid XML: "+derr.Error())
		return
	}
	writeJSON(w, http.StatusOK, cfg)
}

// handleBucketSetLifecycle serves PUT /admin/v1/buckets/{bucket}/lifecycle
// (US-004). Body: LifecycleConfigJSON. Validates structure (rule count,
// status, day/date exclusivity), marshals to XML, runs lifecycle.Parse to
// confirm the worker will accept it, then persists via SetBucketLifecycle.
// Audit row: admin:SetBucketLifecycle.
func (s *Server) handleBucketSetLifecycle(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	name := r.PathValue("bucket")
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "bucket name is required")
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
	var req LifecycleConfigJSON
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "malformed JSON body")
		return
	}
	if len(req.Rules) == 0 {
		writeJSONError(w, http.StatusBadRequest, "InvalidArgument",
			"at least one rule is required")
		return
	}
	for i := range req.Rules {
		if vErr := validateLifecycleRule(&req.Rules[i]); vErr != nil {
			writeJSONError(w, http.StatusBadRequest, "InvalidArgument", vErr.Error())
			return
		}
	}

	xmlBlob, err := encodeLifecycleXML(&req)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	// Confirm the worker will accept the rendered XML before we persist
	// it — catches any case where our JSON shape skipped a required tag.
	if _, perr := lifecycle.Parse(xmlBlob); perr != nil {
		writeJSONError(w, http.StatusBadRequest, "InvalidArgument",
			"lifecycle parse failed: "+perr.Error())
		return
	}

	ctx := r.Context()
	owner := auth.FromContext(ctx).Owner
	s3api.SetAuditOverride(ctx, "admin:SetBucketLifecycle", "bucket:"+name, name, owner)

	b, err := s.Meta.GetBucket(ctx, name)
	if err != nil {
		if errors.Is(err, meta.ErrBucketNotFound) {
			writeJSONError(w, http.StatusNotFound, "NoSuchBucket", "bucket not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	if err := s.Meta.SetBucketLifecycle(ctx, b.ID, xmlBlob); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, req)
}

// validateLifecycleRule applies the AC-level constraints the visual editor
// also enforces client-side. Centralised so the JSON tab path (raw paste)
// fails the same way as the form path.
func validateLifecycleRule(r *LifecycleRuleJSON) error {
	id := strings.TrimSpace(r.ID)
	if id == "" {
		return fmt.Errorf("rule.id is required")
	}
	switch strings.ToLower(strings.TrimSpace(r.Status)) {
	case "enabled":
		r.Status = "Enabled"
	case "disabled":
		r.Status = "Disabled"
	default:
		return fmt.Errorf("rule.status must be Enabled or Disabled (rule %q)", id)
	}
	hasAction := false
	if r.Expiration != nil {
		if r.Expiration.Days > 0 && r.Expiration.Date != "" {
			return fmt.Errorf("rule %q: expiration days and date are mutually exclusive", id)
		}
		if r.Expiration.Days <= 0 && r.Expiration.Date == "" && !r.Expiration.ExpiredObjectDeleteMarker {
			return fmt.Errorf("rule %q: expiration requires days, date, or expired_object_delete_marker", id)
		}
		hasAction = true
	}
	for j := range r.Transitions {
		t := &r.Transitions[j]
		if t.StorageClass == "" {
			return fmt.Errorf("rule %q: transition[%d].storage_class is required", id, j)
		}
		if t.Days > 0 && t.Date != "" {
			return fmt.Errorf("rule %q: transition[%d] days and date are mutually exclusive", id, j)
		}
		if t.Days <= 0 && t.Date == "" {
			return fmt.Errorf("rule %q: transition[%d] requires days or date", id, j)
		}
		hasAction = true
	}
	if r.NoncurrentVersionExpiration != nil {
		if r.NoncurrentVersionExpiration.NoncurrentDays <= 0 {
			return fmt.Errorf("rule %q: noncurrent_version_expiration.noncurrent_days must be > 0", id)
		}
		hasAction = true
	}
	for j := range r.NoncurrentVersionTransitions {
		t := &r.NoncurrentVersionTransitions[j]
		if t.NoncurrentDays <= 0 {
			return fmt.Errorf("rule %q: noncurrent_version_transition[%d].noncurrent_days must be > 0", id, j)
		}
		if t.StorageClass == "" {
			return fmt.Errorf("rule %q: noncurrent_version_transition[%d].storage_class is required", id, j)
		}
		hasAction = true
	}
	if r.AbortIncompleteMultipartUpload != nil {
		if r.AbortIncompleteMultipartUpload.DaysAfterInitiation <= 0 {
			return fmt.Errorf("rule %q: abort_incomplete_multipart_upload.days_after_initiation must be > 0", id)
		}
		hasAction = true
	}
	if !hasAction {
		return fmt.Errorf("rule %q: at least one action is required", id)
	}
	return nil
}

// LifecycleConfigJSON is the operator-console wire shape for bucket lifecycle.
// Mirrors the AWS JSON form of LifecycleConfiguration. The admin handler
// marshals this into the AWS XML the s3api consumers (lifecycle worker) read.
type LifecycleConfigJSON struct {
	Rules []LifecycleRuleJSON `json:"rules"`
}

type LifecycleRuleJSON struct {
	ID                             string                        `json:"id"`
	Status                         string                        `json:"status"`
	Filter                         *LifecycleFilterJSON          `json:"filter,omitempty"`
	Prefix                         string                        `json:"prefix,omitempty"`
	Expiration                     *LifecycleExpirationJSON      `json:"expiration,omitempty"`
	Transitions                    []LifecycleTransitionJSON     `json:"transitions,omitempty"`
	NoncurrentVersionExpiration    *NoncurrentExpirationJSON     `json:"noncurrent_version_expiration,omitempty"`
	NoncurrentVersionTransitions   []NoncurrentTransitionJSON    `json:"noncurrent_version_transitions,omitempty"`
	AbortIncompleteMultipartUpload *AbortIncompleteMultipartJSON `json:"abort_incomplete_multipart_upload,omitempty"`
}

type LifecycleFilterJSON struct {
	Prefix string             `json:"prefix,omitempty"`
	Tags   []LifecycleTagJSON `json:"tags,omitempty"`
}

type LifecycleTagJSON struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type LifecycleExpirationJSON struct {
	Days                      int    `json:"days,omitempty"`
	Date                      string `json:"date,omitempty"`
	ExpiredObjectDeleteMarker bool   `json:"expired_object_delete_marker,omitempty"`
}

type LifecycleTransitionJSON struct {
	Days         int    `json:"days,omitempty"`
	Date         string `json:"date,omitempty"`
	StorageClass string `json:"storage_class"`
}

type NoncurrentExpirationJSON struct {
	NoncurrentDays int `json:"noncurrent_days"`
}

type NoncurrentTransitionJSON struct {
	NoncurrentDays int    `json:"noncurrent_days"`
	StorageClass   string `json:"storage_class"`
}

type AbortIncompleteMultipartJSON struct {
	DaysAfterInitiation int `json:"days_after_initiation"`
}

// lifecycleConfigXML is the AWS LifecycleConfiguration XML wire shape, full
// enough to support the visual editor's rule surface (Filter+Tags via And,
// Days OR Date, ExpiredObjectDeleteMarker, Noncurrent*, AbortIncomplete).
type lifecycleConfigXML struct {
	XMLName xml.Name             `xml:"LifecycleConfiguration"`
	Rules   []lifecycleRuleXML   `xml:"Rule"`
}

type lifecycleRuleXML struct {
	ID                             string                          `xml:"ID"`
	Status                         string                          `xml:"Status"`
	Filter                         *lifecycleFilterXML             `xml:"Filter,omitempty"`
	Prefix                         string                          `xml:"Prefix,omitempty"`
	Expiration                     *lifecycleExpirationXML         `xml:"Expiration,omitempty"`
	Transitions                    []lifecycleTransitionXML        `xml:"Transition,omitempty"`
	NoncurrentVersionExpiration    *noncurrentExpirationXML        `xml:"NoncurrentVersionExpiration,omitempty"`
	NoncurrentVersionTransitions   []noncurrentTransitionXML       `xml:"NoncurrentVersionTransition,omitempty"`
	AbortIncompleteMultipartUpload *abortIncompleteMultipartXML    `xml:"AbortIncompleteMultipartUpload,omitempty"`
}

type lifecycleFilterXML struct {
	Prefix string              `xml:"Prefix,omitempty"`
	Tag    *lifecycleTagXML    `xml:"Tag,omitempty"`
	And    *lifecycleAndOpXML  `xml:"And,omitempty"`
}

type lifecycleAndOpXML struct {
	Prefix string             `xml:"Prefix,omitempty"`
	Tags   []lifecycleTagXML  `xml:"Tag"`
}

type lifecycleTagXML struct {
	Key   string `xml:"Key"`
	Value string `xml:"Value"`
}

type lifecycleExpirationXML struct {
	Days                      int    `xml:"Days,omitempty"`
	Date                      string `xml:"Date,omitempty"`
	ExpiredObjectDeleteMarker bool   `xml:"ExpiredObjectDeleteMarker,omitempty"`
}

type lifecycleTransitionXML struct {
	Days         int    `xml:"Days,omitempty"`
	Date         string `xml:"Date,omitempty"`
	StorageClass string `xml:"StorageClass"`
}

type noncurrentExpirationXML struct {
	NoncurrentDays int `xml:"NoncurrentDays"`
}

type noncurrentTransitionXML struct {
	NoncurrentDays int    `xml:"NoncurrentDays"`
	StorageClass   string `xml:"StorageClass"`
}

type abortIncompleteMultipartXML struct {
	DaysAfterInitiation int `xml:"DaysAfterInitiation"`
}

// encodeLifecycleXML translates the JSON wire shape to the AWS XML the s3api
// consumers store. Empty filters are emitted as <Filter></Filter> per AWS
// shape; combined Prefix+Tags bundle into <And>.
func encodeLifecycleXML(cfg *LifecycleConfigJSON) ([]byte, error) {
	out := lifecycleConfigXML{}
	for _, r := range cfg.Rules {
		xr := lifecycleRuleXML{
			ID:     strings.TrimSpace(r.ID),
			Status: r.Status,
			Prefix: r.Prefix,
		}
		if r.Filter != nil {
			xr.Filter = &lifecycleFilterXML{}
			tagCount := len(r.Filter.Tags)
			switch {
			case r.Filter.Prefix != "" && tagCount > 0:
				and := &lifecycleAndOpXML{Prefix: r.Filter.Prefix}
				for _, t := range r.Filter.Tags {
					and.Tags = append(and.Tags, lifecycleTagXML{Key: t.Key, Value: t.Value})
				}
				xr.Filter.And = and
			case tagCount > 1:
				and := &lifecycleAndOpXML{}
				for _, t := range r.Filter.Tags {
					and.Tags = append(and.Tags, lifecycleTagXML{Key: t.Key, Value: t.Value})
				}
				xr.Filter.And = and
			case tagCount == 1:
				xr.Filter.Tag = &lifecycleTagXML{Key: r.Filter.Tags[0].Key, Value: r.Filter.Tags[0].Value}
			default:
				xr.Filter.Prefix = r.Filter.Prefix
			}
		}
		if r.Expiration != nil {
			xr.Expiration = &lifecycleExpirationXML{
				Days:                      r.Expiration.Days,
				Date:                      r.Expiration.Date,
				ExpiredObjectDeleteMarker: r.Expiration.ExpiredObjectDeleteMarker,
			}
		}
		for _, t := range r.Transitions {
			xr.Transitions = append(xr.Transitions, lifecycleTransitionXML{
				Days:         t.Days,
				Date:         t.Date,
				StorageClass: t.StorageClass,
			})
		}
		if r.NoncurrentVersionExpiration != nil {
			xr.NoncurrentVersionExpiration = &noncurrentExpirationXML{
				NoncurrentDays: r.NoncurrentVersionExpiration.NoncurrentDays,
			}
		}
		for _, t := range r.NoncurrentVersionTransitions {
			xr.NoncurrentVersionTransitions = append(xr.NoncurrentVersionTransitions, noncurrentTransitionXML{
				NoncurrentDays: t.NoncurrentDays,
				StorageClass:   t.StorageClass,
			})
		}
		if r.AbortIncompleteMultipartUpload != nil {
			xr.AbortIncompleteMultipartUpload = &abortIncompleteMultipartXML{
				DaysAfterInitiation: r.AbortIncompleteMultipartUpload.DaysAfterInitiation,
			}
		}
		out.Rules = append(out.Rules, xr)
	}
	return xml.Marshal(out)
}

// decodeLifecycleXML reverses encodeLifecycleXML for the GET path. Keeps the
// shape lossless across the round-trip so the JSON tab editor can re-render
// what was just saved.
func decodeLifecycleXML(blob []byte) (*LifecycleConfigJSON, error) {
	var x lifecycleConfigXML
	if err := xml.Unmarshal(blob, &x); err != nil {
		return nil, err
	}
	out := &LifecycleConfigJSON{}
	for _, r := range x.Rules {
		jr := LifecycleRuleJSON{
			ID:     r.ID,
			Status: r.Status,
			Prefix: r.Prefix,
		}
		if r.Filter != nil {
			f := &LifecycleFilterJSON{Prefix: r.Filter.Prefix}
			if r.Filter.Tag != nil {
				f.Tags = append(f.Tags, LifecycleTagJSON{Key: r.Filter.Tag.Key, Value: r.Filter.Tag.Value})
			}
			if r.Filter.And != nil {
				if f.Prefix == "" {
					f.Prefix = r.Filter.And.Prefix
				}
				for _, t := range r.Filter.And.Tags {
					f.Tags = append(f.Tags, LifecycleTagJSON{Key: t.Key, Value: t.Value})
				}
			}
			jr.Filter = f
		}
		if r.Expiration != nil {
			jr.Expiration = &LifecycleExpirationJSON{
				Days:                      r.Expiration.Days,
				Date:                      r.Expiration.Date,
				ExpiredObjectDeleteMarker: r.Expiration.ExpiredObjectDeleteMarker,
			}
		}
		for _, t := range r.Transitions {
			jr.Transitions = append(jr.Transitions, LifecycleTransitionJSON{
				Days:         t.Days,
				Date:         t.Date,
				StorageClass: t.StorageClass,
			})
		}
		if r.NoncurrentVersionExpiration != nil {
			jr.NoncurrentVersionExpiration = &NoncurrentExpirationJSON{
				NoncurrentDays: r.NoncurrentVersionExpiration.NoncurrentDays,
			}
		}
		for _, t := range r.NoncurrentVersionTransitions {
			jr.NoncurrentVersionTransitions = append(jr.NoncurrentVersionTransitions, NoncurrentTransitionJSON{
				NoncurrentDays: t.NoncurrentDays,
				StorageClass:   t.StorageClass,
			})
		}
		if r.AbortIncompleteMultipartUpload != nil {
			jr.AbortIncompleteMultipartUpload = &AbortIncompleteMultipartJSON{
				DaysAfterInitiation: r.AbortIncompleteMultipartUpload.DaysAfterInitiation,
			}
		}
		out.Rules = append(out.Rules, jr)
	}
	return out, nil
}
