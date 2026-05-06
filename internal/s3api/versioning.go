package s3api

import (
	"encoding/xml"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/danchupin/strata/internal/meta"
)

func (s *Server) getBucketVersioning(w http.ResponseWriter, r *http.Request, bucket string) {
	b, err := s.Meta.GetBucket(r.Context(), bucket)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	status := b.Versioning
	if status == meta.VersioningDisabled {
		status = ""
	}
	writeXML(w, http.StatusOK, versioningConfiguration{Status: status, MfaDelete: b.MfaDelete})
}

func (s *Server) putBucketVersioning(w http.ResponseWriter, r *http.Request, bucket string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		writeError(w, r, ErrMalformedXML)
		return
	}
	var doc versioningConfiguration
	if err := xml.Unmarshal(body, &doc); err != nil {
		writeError(w, r, ErrMalformedXML)
		return
	}
	state := doc.Status
	switch state {
	case meta.VersioningEnabled, meta.VersioningSuspended:
	case "":
		state = meta.VersioningDisabled
	default:
		writeError(w, r, ErrInvalidArgument)
		return
	}
	mfa := doc.MfaDelete
	switch mfa {
	case meta.MfaDeleteEnabled, meta.MfaDeleteDisabled, "":
	default:
		writeError(w, r, ErrInvalidArgument)
		return
	}
	if err := s.Meta.SetBucketVersioning(r.Context(), bucket, state); err != nil {
		mapMetaErr(w, r, err)
		return
	}
	if mfa != "" {
		if err := s.Meta.SetBucketMfaDelete(r.Context(), bucket, mfa); err != nil {
			mapMetaErr(w, r, err)
			return
		}
	}
	w.WriteHeader(http.StatusOK)
}

// wireVersionID returns the wire form of a version-id: meta.NullVersionLiteral
// ("null") for rows flagged IsNull (the bucket's null-versioned row), and the
// stored TimeUUID otherwise. Use it everywhere a VersionID is rendered into
// XML or response headers so clients see "null" for the AWS-spec null version
// rather than the zero-UUID sentinel.
func wireVersionID(o *meta.Object) string {
	if o == nil {
		return ""
	}
	if o.IsNull {
		return meta.NullVersionLiteral
	}
	return o.VersionID
}

// nextWireVersionID renders the continuation marker for a paginated
// ListObjectVersions response. The meta backend returns the raw VersionID of
// the last row on the page; we translate the sentinel to "null" so the
// next request can pass it back unchanged via key-version-id-marker.
func nextWireVersionID(res *meta.ListVersionsResult) string {
	if res == nil || res.NextVersionID == "" {
		return ""
	}
	if res.NextVersionID == meta.NullVersionID {
		return meta.NullVersionLiteral
	}
	return res.NextVersionID
}

func (s *Server) listObjectVersions(w http.ResponseWriter, r *http.Request, bucket string) {
	b, err := s.Meta.GetBucket(r.Context(), bucket)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("max-keys"))
	if limit <= 0 {
		limit = 1000
	}
	res, err := s.Meta.ListObjectVersions(r.Context(), b.ID, meta.ListOptions{
		Prefix:    q.Get("prefix"),
		Delimiter: q.Get("delimiter"),
		Marker:    q.Get("key-marker"),
		Limit:     limit,
	})
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	resp := listVersionsResult{
		Name:            bucket,
		Prefix:          q.Get("prefix"),
		Delimiter:       q.Get("delimiter"),
		KeyMarker:       q.Get("key-marker"),
		MaxKeys:         limit,
		IsTruncated:     res.Truncated,
		NextKeyMarker:   res.NextKeyMarker,
		NextVersionID:   nextWireVersionID(res),
	}
	ownerEntry := &owner{ID: b.Owner, DisplayName: b.Owner}
	for _, v := range res.Versions {
		if v.IsDeleteMarker {
			resp.DeleteMarkers = append(resp.DeleteMarkers, deleteMarkerEntry{
				Key:          v.Key,
				VersionID:    wireVersionID(v),
				IsLatest:     v.IsLatest,
				LastModified: v.Mtime.UTC().Format(time.RFC3339),
				Owner:        ownerEntry,
			})
		} else {
			resp.Versions = append(resp.Versions, versionEntry{
				Key:          v.Key,
				VersionID:    wireVersionID(v),
				IsLatest:     v.IsLatest,
				LastModified: v.Mtime.UTC().Format(time.RFC3339),
				ETag:         `"` + v.ETag + `"`,
				Size:         v.Size,
				StorageClass: v.StorageClass,
				Owner:        ownerEntry,
			})
		}
	}
	for _, p := range res.CommonPrefixes {
		resp.CommonPrefixes = append(resp.CommonPrefixes, commonPrefixEl{Prefix: p})
	}
	writeXML(w, http.StatusOK, resp)
}
