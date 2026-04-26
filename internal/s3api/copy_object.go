package s3api

import (
	"encoding/xml"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/danchupin/strata/internal/meta"
)

type copyObjectResult struct {
	XMLName      xml.Name `xml:"CopyObjectResult"`
	ETag         string   `xml:"ETag"`
	LastModified string   `xml:"LastModified"`
}

func parseCopySource(raw string) (bucket, key, versionID string, ok bool) {
	if raw == "" {
		return "", "", "", false
	}
	if decoded, err := url.PathUnescape(raw); err == nil {
		raw = decoded
	}
	raw = strings.TrimPrefix(raw, "/")
	if q := strings.Index(raw, "?"); q >= 0 {
		params, err := url.ParseQuery(raw[q+1:])
		if err == nil {
			versionID = params.Get("versionId")
		}
		raw = raw[:q]
	}
	slash := strings.Index(raw, "/")
	if slash <= 0 || slash == len(raw)-1 {
		return "", "", "", false
	}
	return raw[:slash], raw[slash+1:], versionID, true
}

func (s *Server) copyObject(w http.ResponseWriter, r *http.Request, dstBucket *meta.Bucket, dstKey string) {
	srcBucket, srcKey, srcVersion, ok := parseCopySource(r.Header.Get("x-amz-copy-source"))
	if !ok {
		writeError(w, r, ErrInvalidArgument)
		return
	}

	sb, err := s.Meta.GetBucket(r.Context(), srcBucket)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	srcObj, err := s.Meta.GetObject(r.Context(), sb.ID, srcKey, srcVersion)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}

	srcSSEC, srcSSECErr, srcSSECOK := parseCopySourceSSECHeaders(r)
	if !srcSSECOK {
		writeError(w, r, srcSSECErr)
		return
	}
	if srcObj.SSECKeyMD5 != "" {
		if !srcSSEC.Present {
			writeError(w, r, ErrSSECRequired)
			return
		}
		if srcSSEC.KeyMD5 != srcObj.SSECKeyMD5 {
			writeError(w, r, ErrSSECKeyMismatch)
			return
		}
	}
	if err := checkCopySourceConditional(r.Header, srcObj.ETag, srcObj.Mtime); err != nil {
		writeError(w, r, *err)
		return
	}

	dstSSEC, dstSSECErr, dstSSECOK := parseSSECHeaders(r)
	if !dstSSECOK {
		writeError(w, r, dstSSECErr)
		return
	}

	metadataDirective, mderr := parseDirective(r.Header.Get("x-amz-metadata-directive"))
	if mderr != nil {
		writeError(w, r, *mderr)
		return
	}
	taggingDirective, tderr := parseDirective(r.Header.Get("x-amz-tagging-directive"))
	if tderr != nil {
		writeError(w, r, *tderr)
		return
	}

	sameBucket := dstBucket.ID == sb.ID
	if sameBucket && srcKey == dstKey && srcVersion == "" && metadataDirective == "COPY" && taggingDirective == "COPY" {
		writeError(w, r, ErrInvalidArgument)
		return
	}

	class := r.Header.Get("x-amz-storage-class")
	if class == "" {
		class = srcObj.StorageClass
		if class == "" {
			class = dstBucket.DefaultClass
		}
	}

	rc, err := s.Data.GetChunks(r.Context(), srcObj.Manifest, 0, srcObj.Size)
	if err != nil {
		writeError(w, r, ErrInternal)
		return
	}
	defer rc.Close()

	m, err := s.Data.PutChunks(r.Context(), rc, class)
	if err != nil {
		if strings.Contains(err.Error(), "unknown storage class") {
			writeError(w, r, ErrInvalidStorageClass)
			return
		}
		writeError(w, r, ErrInternal)
		return
	}

	obj := &meta.Object{
		BucketID:     dstBucket.ID,
		Key:          dstKey,
		Size:         m.Size,
		ETag:         m.ETag,
		StorageClass: m.Class,
		Mtime:        time.Now().UTC(),
		Manifest:     m,
	}
	if dstSSEC.Present {
		obj.SSECKeyMD5 = dstSSEC.KeyMD5
	}

	if metadataDirective == "REPLACE" {
		obj.ContentType = r.Header.Get("Content-Type")
		obj.UserMeta = extractUserMeta(r.Header)
	} else {
		obj.ContentType = srcObj.ContentType
		if len(srcObj.UserMeta) > 0 {
			obj.UserMeta = copyStringMap(srcObj.UserMeta)
		}
	}

	if taggingDirective == "REPLACE" {
		if tagHdr := r.Header.Get("x-amz-tagging"); tagHdr != "" {
			obj.Tags = parseTagHeader(tagHdr)
		}
	} else if len(srcObj.Tags) > 0 {
		obj.Tags = copyStringMap(srcObj.Tags)
	}

	if rm := r.Header.Get("x-amz-object-lock-mode"); rm != "" {
		obj.RetainMode = rm
	}
	if ru := r.Header.Get("x-amz-object-lock-retain-until-date"); ru != "" {
		if t, err := time.Parse(time.RFC3339, ru); err == nil {
			obj.RetainUntil = t
		}
	}
	if r.Header.Get("x-amz-object-lock-legal-hold") == "ON" {
		obj.LegalHold = true
	}

	if err := s.Meta.PutObject(r.Context(), obj, meta.IsVersioningActive(dstBucket.Versioning)); err != nil {
		_ = s.Data.Delete(r.Context(), m)
		mapMetaErr(w, r, err)
		return
	}

	if meta.IsVersioningActive(dstBucket.Versioning) && obj.VersionID != "" {
		w.Header().Set("x-amz-version-id", obj.VersionID)
	}
	if srcObj.VersionID != "" {
		w.Header().Set("x-amz-copy-source-version-id", srcObj.VersionID)
	}

	writeXML(w, http.StatusOK, copyObjectResult{
		ETag:         `"` + m.ETag + `"`,
		LastModified: obj.Mtime.UTC().Format(time.RFC3339),
	})
}

func parseDirective(raw string) (string, *APIError) {
	v := strings.ToUpper(strings.TrimSpace(raw))
	switch v {
	case "":
		return "COPY", nil
	case "COPY", "REPLACE":
		return v, nil
	default:
		return "", &ErrInvalidArgument
	}
}

func checkCopySourceConditional(h http.Header, etag string, mtime time.Time) *APIError {
	quoted := `"` + strings.Trim(etag, `"`) + `"`
	if v := h.Get("x-amz-copy-source-if-match"); v != "" {
		if !etagMatches(v, quoted) {
			return &ErrPreconditionFailed
		}
	}
	if v := h.Get("x-amz-copy-source-if-none-match"); v != "" {
		if etagMatches(v, quoted) {
			return &ErrPreconditionFailed
		}
	}
	if v := h.Get("x-amz-copy-source-if-unmodified-since"); v != "" {
		if t, err := http.ParseTime(v); err == nil && mtime.After(t.Add(time.Second)) {
			return &ErrPreconditionFailed
		}
	}
	if v := h.Get("x-amz-copy-source-if-modified-since"); v != "" {
		if t, err := http.ParseTime(v); err == nil && !mtime.After(t) {
			return &ErrPreconditionFailed
		}
	}
	return nil
}

func extractUserMeta(h http.Header) map[string]string {
	out := map[string]string{}
	for k, vs := range h {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "x-amz-meta-") && len(vs) > 0 {
			out[strings.TrimPrefix(lk, "x-amz-meta-")] = vs[0]
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func copyStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

