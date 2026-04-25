package s3api

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/danchupin/strata/internal/meta"
)

// RestoreRequest mirrors the AWS POST ?restore body.
type restoreRequest struct {
	XMLName xml.Name `xml:"RestoreRequest"`
	Days    int      `xml:"Days"`
	Tier    string   `xml:"Tier"`
}

func (s *Server) postObjectRestore(w http.ResponseWriter, r *http.Request, b *meta.Bucket, key string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, r, ErrInternal)
		return
	}
	days := 1
	if len(body) > 0 {
		var req restoreRequest
		if err := xml.Unmarshal(body, &req); err != nil {
			writeError(w, r, ErrMalformedXML)
			return
		}
		if req.Days > 0 {
			days = req.Days
		}
	}
	versionID := r.URL.Query().Get("versionId")
	o, err := s.Meta.GetObject(r.Context(), b.ID, key, versionID)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	expiry := time.Now().UTC().Add(time.Duration(days) * 24 * time.Hour)
	status := formatRestoreHeader(false, expiry)
	if err := s.Meta.SetObjectRestoreStatus(r.Context(), b.ID, key, o.VersionID, status); err != nil {
		if errors.Is(err, meta.ErrObjectNotFound) {
			writeError(w, r, ErrNoSuchKey)
			return
		}
		writeError(w, r, ErrInternal)
		return
	}
	w.Header().Set("x-amz-restore-output-path", "")
	w.WriteHeader(http.StatusOK)
}

func formatRestoreHeader(ongoing bool, expiry time.Time) string {
	return fmt.Sprintf(`ongoing-request="%t", expiry-date="%s"`, ongoing, expiry.Format(http.TimeFormat))
}
