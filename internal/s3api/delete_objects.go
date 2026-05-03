package s3api

import (
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/danchupin/strata/internal/meta"
)

type deleteObjectsRequest struct {
	XMLName xml.Name               `xml:"Delete"`
	Quiet   bool                   `xml:"Quiet"`
	Objects []deleteObjectsObjectE `xml:"Object"`
}

type deleteObjectsObjectE struct {
	Key       string `xml:"Key"`
	VersionID string `xml:"VersionId"`
}

type deleteObjectsResult struct {
	XMLName xml.Name                 `xml:"DeleteResult"`
	Deleted []deleteObjectsDeletedE  `xml:"Deleted"`
	Errors  []deleteObjectsErrorEntE `xml:"Error"`
}

type deleteObjectsDeletedE struct {
	Key                   string `xml:"Key"`
	VersionID             string `xml:"VersionId,omitempty"`
	DeleteMarker          bool   `xml:"DeleteMarker,omitempty"`
	DeleteMarkerVersionID string `xml:"DeleteMarkerVersionId,omitempty"`
}

type deleteObjectsErrorEntE struct {
	Key       string `xml:"Key"`
	VersionID string `xml:"VersionId,omitempty"`
	Code      string `xml:"Code"`
	Message   string `xml:"Message"`
}

func (s *Server) deleteObjects(w http.ResponseWriter, r *http.Request, bucket string) {
	b, err := s.Meta.GetBucket(r.Context(), bucket)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, r, ErrMalformedXML)
		return
	}
	var req deleteObjectsRequest
	if err := xml.Unmarshal(body, &req); err != nil {
		writeError(w, r, ErrMalformedXML)
		return
	}
	if len(req.Objects) == 0 || len(req.Objects) > 1000 {
		writeError(w, r, ErrMalformedXML)
		return
	}

	versioned := meta.IsVersioningActive(b.Versioning)
	bypassGovernance := strings.EqualFold(r.Header.Get("x-amz-bypass-governance-retention"), "true")

	result := deleteObjectsResult{}
	for _, it := range req.Objects {
		if it.Key == "" {
			result.Errors = append(result.Errors, deleteObjectsErrorEntE{
				Key: it.Key, Code: "MalformedXML", Message: "missing key",
			})
			continue
		}

		if it.VersionID != "" {
			if existing, err := s.Meta.GetObject(r.Context(), b.ID, it.Key, it.VersionID); err == nil {
				if objectLockBlocksDelete(existing, bypassGovernance) {
					result.Errors = append(result.Errors, deleteObjectsErrorEntE{
						Key: it.Key, VersionID: it.VersionID,
						Code: "AccessDenied", Message: "Object is protected by object lock",
					})
					continue
				}
			}
		} else if !versioned {
			if existing, err := s.Meta.GetObject(r.Context(), b.ID, it.Key, ""); err == nil {
				if objectLockBlocksDelete(existing, bypassGovernance) {
					result.Errors = append(result.Errors, deleteObjectsErrorEntE{
						Key: it.Key,
						Code: "AccessDenied", Message: "Object is protected by object lock",
					})
					continue
				}
			}
		}

		var (
			o   *meta.Object
			derr error
		)
		if it.VersionID == "" && b.Versioning == meta.VersioningSuspended {
			if prior, perr := s.Meta.GetObject(r.Context(), b.ID, it.Key, meta.NullVersionLiteral); perr == nil && prior != nil && prior.Manifest != nil {
				s.enqueueChunks(r.Context(), prior.Manifest.Chunks)
			}
			o, derr = s.Meta.DeleteObjectNullReplacement(r.Context(), b.ID, it.Key)
		} else {
			o, derr = s.Meta.DeleteObject(r.Context(), b.ID, it.Key, it.VersionID, versioned)
		}
		if derr != nil && !errors.Is(derr, meta.ErrObjectNotFound) {
			result.Errors = append(result.Errors, deleteObjectsErrorEntE{
				Key: it.Key, VersionID: it.VersionID,
				Code: "InternalError", Message: derr.Error(),
			})
			continue
		}

		if (it.VersionID != "" || !versioned) && o != nil && o.Manifest != nil {
			s.enqueueOrphan(r.Context(), o.Manifest)
		}

		entry := deleteObjectsDeletedE{Key: it.Key, VersionID: it.VersionID}
		if o != nil && versioned {
			if o.IsDeleteMarker {
				entry.DeleteMarker = true
				entry.DeleteMarkerVersionID = wireVersionID(o)
			} else if it.VersionID != "" {
				entry.VersionID = wireVersionID(o)
			}
		}
		if !req.Quiet {
			result.Deleted = append(result.Deleted, entry)
		}
	}

	writeXML(w, http.StatusOK, result)
}
