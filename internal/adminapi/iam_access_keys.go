package adminapi

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/s3api"
)

// IAMAccessKeySummary is the wire shape for one entry in
// GET /admin/v1/iam/users/{userName}/access-keys. SecretAccessKey is NEVER
// included — admins fetch it once at creation time only.
type IAMAccessKeySummary struct {
	AccessKeyID string `json:"access_key_id"`
	UserName    string `json:"user_name"`
	CreatedAt   int64  `json:"created_at"`
	Disabled    bool   `json:"disabled"`
}

// IAMAccessKeyListResponse wraps a list of summaries.
type IAMAccessKeyListResponse struct {
	AccessKeys []IAMAccessKeySummary `json:"access_keys"`
}

// IAMAccessKeyCreateResponse is the one-shot response carrying both the
// access key and its secret. Callers MUST persist secret_key client-side
// immediately — the server cannot recover it after this response is dropped.
type IAMAccessKeyCreateResponse struct {
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	UserName        string `json:"user_name"`
	CreatedAt       int64  `json:"created_at"`
	Disabled        bool   `json:"disabled"`
}

// IAMAccessKeyUpdateRequest is the JSON body for PATCH on an access key.
// Only the disabled flag is mutable today.
type IAMAccessKeyUpdateRequest struct {
	Disabled bool `json:"disabled"`
}

// handleIAMAccessKeyList serves GET /admin/v1/iam/users/{userName}/access-keys.
// Lists the keys owned by userName via meta.Store.ListIAMAccessKeys. Excludes
// SecretAccessKey from every row; the secret is recoverable only at creation
// time.
func (s *Server) handleIAMAccessKeyList(w http.ResponseWriter, r *http.Request) {
	if s.Meta == nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", "meta store not configured")
		return
	}
	name := r.PathValue("userName")
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "user_name is required")
		return
	}
	if _, gerr := s.Meta.GetIAMUser(r.Context(), name); gerr != nil {
		if errors.Is(gerr, meta.ErrIAMUserNotFound) {
			writeJSONError(w, http.StatusNotFound, "NoSuchEntity", "user not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", gerr.Error())
		return
	}
	keys, kerr := s.Meta.ListIAMAccessKeys(r.Context(), name)
	if kerr != nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", kerr.Error())
		return
	}
	out := IAMAccessKeyListResponse{AccessKeys: make([]IAMAccessKeySummary, 0, len(keys))}
	for _, ak := range keys {
		out.AccessKeys = append(out.AccessKeys, IAMAccessKeySummary{
			AccessKeyID: ak.AccessKeyID,
			UserName:    ak.UserName,
			CreatedAt:   ak.CreatedAt.Unix(),
			Disabled:    ak.Disabled,
		})
	}
	sort.Slice(out.AccessKeys, func(i, j int) bool {
		return out.AccessKeys[i].AccessKeyID < out.AccessKeys[j].AccessKeyID
	})
	writeJSON(w, http.StatusOK, out)
}

// handleIAMAccessKeyCreate serves POST /admin/v1/iam/users/{userName}/access-keys.
// Mints a fresh access key + secret pair and returns BOTH in the response. The
// secret is only ever returned here — subsequent reads omit it. Audit:
// admin:CreateAccessKey, resource iam-access-key:<accessKeyID>.
func (s *Server) handleIAMAccessKeyCreate(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	if s.Meta == nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", "meta store not configured")
		return
	}
	name := r.PathValue("userName")
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "user_name is required")
		return
	}
	if _, gerr := s.Meta.GetIAMUser(r.Context(), name); gerr != nil {
		if errors.Is(gerr, meta.ErrIAMUserNotFound) {
			writeJSONError(w, http.StatusNotFound, "NoSuchEntity", "user not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", gerr.Error())
		return
	}
	ctx := r.Context()
	owner := auth.FromContext(ctx).Owner
	ak := &meta.IAMAccessKey{
		AccessKeyID:     newAccessKeyID(),
		SecretAccessKey: newSecretAccessKey(),
		UserName:        name,
		CreatedAt:       time.Now().UTC(),
	}
	if cerr := s.Meta.CreateIAMAccessKey(ctx, ak); cerr != nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", cerr.Error())
		return
	}
	s3api.SetAuditOverride(ctx, "admin:CreateAccessKey", "iam-access-key:"+ak.AccessKeyID, "", owner)
	writeJSON(w, http.StatusCreated, IAMAccessKeyCreateResponse{
		AccessKeyID:     ak.AccessKeyID,
		SecretAccessKey: ak.SecretAccessKey,
		UserName:        ak.UserName,
		CreatedAt:       ak.CreatedAt.Unix(),
		Disabled:        ak.Disabled,
	})
}

// handleIAMAccessKeyUpdate serves PATCH /admin/v1/iam/access-keys/{accessKey}.
// Today the only mutable field is Disabled. Stamps the audit row as either
// admin:DisableAccessKey or admin:EnableAccessKey depending on the new state.
// Calls InvalidateCredential after a successful flip so the gateway's
// auth.MultiStore cache drops the now-stale entry — without this, a disabled
// key could keep working until DefaultCacheTTL elapses.
func (s *Server) handleIAMAccessKeyUpdate(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	if s.Meta == nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", "meta store not configured")
		return
	}
	id := r.PathValue("accessKey")
	if id == "" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "access_key is required")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<10))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "failed to read body")
		return
	}
	var req IAMAccessKeyUpdateRequest
	if jerr := json.Unmarshal(body, &req); jerr != nil {
		writeJSONError(w, http.StatusBadRequest, "MalformedRequest", "invalid JSON: "+jerr.Error())
		return
	}
	ak, uerr := s.Meta.UpdateIAMAccessKeyDisabled(r.Context(), id, req.Disabled)
	if uerr != nil {
		if errors.Is(uerr, meta.ErrIAMAccessKeyNotFound) {
			writeJSONError(w, http.StatusNotFound, "NoSuchEntity", "access key not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", uerr.Error())
		return
	}
	if s.InvalidateCredential != nil {
		s.InvalidateCredential(id)
	}
	action := "admin:EnableAccessKey"
	if req.Disabled {
		action = "admin:DisableAccessKey"
	}
	owner := auth.FromContext(r.Context()).Owner
	s3api.SetAuditOverride(r.Context(), action, "iam-access-key:"+id, "", owner)
	writeJSON(w, http.StatusOK, IAMAccessKeySummary{
		AccessKeyID: ak.AccessKeyID,
		UserName:    ak.UserName,
		CreatedAt:   ak.CreatedAt.Unix(),
		Disabled:    ak.Disabled,
	})
}

// handleIAMAccessKeyDelete serves DELETE /admin/v1/iam/access-keys/{accessKey}.
// Drops the row via meta.Store.DeleteIAMAccessKey + invalidates the in-memory
// credential cache. Audit row admin:DeleteAccessKey.
func (s *Server) handleIAMAccessKeyDelete(w http.ResponseWriter, r *http.Request) {
	if s.Meta == nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", "meta store not configured")
		return
	}
	id := r.PathValue("accessKey")
	if id == "" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "access_key is required")
		return
	}
	if _, derr := s.Meta.DeleteIAMAccessKey(r.Context(), id); derr != nil {
		if errors.Is(derr, meta.ErrIAMAccessKeyNotFound) {
			writeJSONError(w, http.StatusNotFound, "NoSuchEntity", "access key not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", derr.Error())
		return
	}
	if s.InvalidateCredential != nil {
		s.InvalidateCredential(id)
	}
	owner := auth.FromContext(r.Context()).Owner
	s3api.SetAuditOverride(r.Context(), "admin:DeleteAccessKey", "iam-access-key:"+id, "", owner)
	w.WriteHeader(http.StatusNoContent)
}

// newAccessKeyID mints a fresh AKIA-prefixed identifier — same shape as the
// s3api.iamCreateAccessKey path so the operator-minted key is indistinguishable
// from a CLI-minted one. 20 hex chars after the prefix gives 80 bits of
// entropy, plenty for collision-free minting at IAM cardinality.
func newAccessKeyID() string {
	var buf [10]byte
	if _, err := rand.Read(buf[:]); err != nil {
		panic(fmt.Sprintf("adminapi: rand.Read: %v", err))
	}
	return "AKIA" + strings.ToUpper(hex.EncodeToString(buf[:]))
}

// newSecretAccessKey mints a 40-byte base64 secret. Same shape as
// s3api.iamCreateAccessKey.
func newSecretAccessKey() string {
	var buf [30]byte
	if _, err := rand.Read(buf[:]); err != nil {
		panic(fmt.Sprintf("adminapi: rand.Read: %v", err))
	}
	return base64.StdEncoding.EncodeToString(buf[:])
}
