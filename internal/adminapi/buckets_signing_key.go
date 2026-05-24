package adminapi

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/crypto/kms"
	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/s3api"
)

// signingKeyRotateRequest is the JSON body for POST
// /admin/v1/buckets/{name}/signing-key/rotate. Both fields are optional;
// omitted KeyID falls back to s.SigningKey.DefaultKeyID (or the bucket
// name when that is also empty — see resolveCMKID).
type signingKeyRotateRequest struct {
	KeyID string `json:"key_id,omitempty"`
}

// signingKeyRotateResponse is the one-shot response carrying the freshly
// generated DEK so the operator can paste it into their client config as
// the SigV4 secret. After this response is discarded the gateway cannot
// recover the plaintext — wrapped form persists on bucket meta and is
// unwrapped per-request via kms.Provider.
type signingKeyRotateResponse struct {
	KeyID            string `json:"key_id"`
	SecretAccessKey  string `json:"secret_access_key"`
	CreatedAt        int64  `json:"created_at"`
	WrappedDEKLength int    `json:"wrapped_dek_length"`
}

// signingKeyStatusResponse is the read-only status surface for GET
// /admin/v1/buckets/{name}/signing-key/status. AgeDays / MaxAgeDays are
// emitted as fractional days (truncated to 2 decimals at JSON encode) so
// operators can spot "rotated yesterday" vs "rotated 88 days ago" at a
// glance. Expired is the precomputed `now - createdAt > maxAge`.
type signingKeyStatusResponse struct {
	KeyID       string  `json:"key_id"`
	CreatedAt   int64   `json:"created_at"`
	AgeDays     float64 `json:"age_days"`
	MaxAgeDays  float64 `json:"max_age_days"`
	Expired     bool    `json:"expired"`
}

// handleBucketSigningKeyRotate serves POST
// /admin/v1/buckets/{bucket}/signing-key/rotate. Generates a fresh DEK
// via kms.Provider.GenerateDataKey, persists the wrapped form on bucket
// meta, invalidates the auth-side DEK cache for the bucket, and returns
// the plaintext DEK (hex) so the operator can use it as the client-side
// SigV4 secret. Audit verb admin:RotateBucketSigningKey.
func (s *Server) handleBucketSigningKeyRotate(w http.ResponseWriter, r *http.Request) {
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
	if s.SigningKey.Provider == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "SigningKeyDisabled",
			"no KMS provider configured; set STRATA_KMS_VAULT_* / STRATA_KMS_AWS_REGION / STRATA_KMS_LOCAL_HSM_SEED to enable per-bucket signing keys")
		return
	}

	var req signingKeyRotateRequest
	if r.ContentLength > 0 {
		body, err := io.ReadAll(io.LimitReader(r.Body, 4<<10))
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "BadRequest", "failed to read body")
			return
		}
		if len(body) > 0 {
			if jerr := json.Unmarshal(body, &req); jerr != nil {
				writeJSONError(w, http.StatusBadRequest, "MalformedRequest", "invalid JSON: "+jerr.Error())
				return
			}
		}
	}
	keyID := s.resolveCMKID(req.KeyID, name)

	ctx := r.Context()
	owner := auth.FromContext(ctx).Owner
	s3api.SetAuditOverride(ctx, "admin:RotateBucketSigningKey", "bucket:"+name, name, owner)

	if _, gerr := s.Meta.GetBucket(ctx, name); gerr != nil {
		if errors.Is(gerr, meta.ErrBucketNotFound) {
			writeJSONError(w, http.StatusNotFound, "NoSuchBucket", "bucket not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", gerr.Error())
		return
	}

	plain, wrapped, kerr := s.SigningKey.Provider.GenerateDataKey(ctx, keyID)
	if kerr != nil {
		s.writeKMSError(w, "RotateBucketSigningKey", kerr)
		return
	}
	defer wipe(plain)

	if err := s.Meta.SetBucketSigningKey(ctx, name, wrapped, keyID); err != nil {
		if errors.Is(err, meta.ErrBucketNotFound) {
			writeJSONError(w, http.StatusNotFound, "NoSuchBucket", "bucket not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	if s.SigningKey.Cache != nil {
		s.SigningKey.Cache.Invalidate(name)
	}

	_, _, createdAt, gerr := s.Meta.GetBucketSigningKey(ctx, name)
	if gerr != nil {
		// Read-after-write fall-back: persist succeeded above; use now()
		// as a defensive approximation.
		createdAt = time.Now().UTC()
	}

	writeJSON(w, http.StatusOK, signingKeyRotateResponse{
		KeyID:            keyID,
		SecretAccessKey:  hex.EncodeToString(plain),
		CreatedAt:        createdAt.Unix(),
		WrappedDEKLength: len(wrapped),
	})
}

// handleBucketSigningKeyStatus serves GET
// /admin/v1/buckets/{bucket}/signing-key/status. Returns 404
// NoSigningKey when none is configured, 200 with age + expired flag
// otherwise. Audit verb admin:GetBucketSigningKeyStatus.
func (s *Server) handleBucketSigningKeyStatus(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("bucket")
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "bucket name is required")
		return
	}
	if s.Meta == nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", "meta store not configured")
		return
	}
	ctx := r.Context()
	owner := auth.FromContext(ctx).Owner
	s3api.SetAuditOverride(ctx, "admin:GetBucketSigningKeyStatus", "bucket:"+name, name, owner)

	_, keyID, createdAt, err := s.Meta.GetBucketSigningKey(ctx, name)
	if err != nil {
		switch {
		case errors.Is(err, meta.ErrBucketNotFound):
			writeJSONError(w, http.StatusNotFound, "NoSuchBucket", "bucket not found")
		case errors.Is(err, meta.ErrBucketSigningKeyNotSet):
			writeJSONError(w, http.StatusNotFound, "NoSigningKey",
				"no per-bucket signing key configured; POST /signing-key/rotate to mint one")
		default:
			writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		}
		return
	}

	maxAge := s.SigningKey.MaxAge
	age := time.Since(createdAt)
	expired := maxAge > 0 && age > maxAge
	writeJSON(w, http.StatusOK, signingKeyStatusResponse{
		KeyID:       keyID,
		CreatedAt:   createdAt.Unix(),
		AgeDays:     daysFromDuration(age),
		MaxAgeDays:  daysFromDuration(maxAge),
		Expired:     expired,
	})
}

// handleBucketSigningKeyDelete serves DELETE
// /admin/v1/buckets/{bucket}/signing-key. Idempotent — 204 on success or
// when no key is configured (matches the placement-delete shape). Drops
// the cached DEK so the next SigV4 attempt falls through to the IAM
// access-key path. Audit verb admin:DeleteBucketSigningKey.
func (s *Server) handleBucketSigningKeyDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("bucket")
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "bucket name is required")
		return
	}
	if s.Meta == nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", "meta store not configured")
		return
	}
	ctx := r.Context()
	owner := auth.FromContext(ctx).Owner
	s3api.SetAuditOverride(ctx, "admin:DeleteBucketSigningKey", "bucket:"+name, name, owner)

	if err := s.Meta.DeleteBucketSigningKey(ctx, name); err != nil {
		if errors.Is(err, meta.ErrBucketNotFound) {
			writeJSONError(w, http.StatusNotFound, "NoSuchBucket", "bucket not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	if s.SigningKey.Cache != nil {
		s.SigningKey.Cache.Invalidate(name)
	}
	w.WriteHeader(http.StatusNoContent)
}

// resolveCMKID picks the operator-supplied key id, then the configured
// default, then the bucket name (which doubles as a sane default for
// both AWS KMS aliases and Vault Transit CMK handles).
func (s *Server) resolveCMKID(operator, bucket string) string {
	if operator != "" {
		return operator
	}
	if s.SigningKey.DefaultKeyID != "" {
		return s.SigningKey.DefaultKeyID
	}
	return bucket
}

// writeKMSError maps a kms.Provider error to one of the operator-facing
// JSON responses. Mirrors the gateway-side s3api.WriteAuthDenied mapping
// so /admin and /<bucket> emit the same shape on the same upstream
// error (US-002 fail-closed).
func (s *Server) writeKMSError(w http.ResponseWriter, action string, err error) {
	if errors.Is(err, kms.ErrKMSUnavailable) {
		w.Header().Set("Retry-After", "30")
		writeJSONError(w, http.StatusServiceUnavailable, "KMSUnavailable",
			"KMS provider unavailable: "+err.Error())
		return
	}
	writeJSONError(w, http.StatusInternalServerError, "KMSError",
		action+": "+err.Error())
}

// daysFromDuration renders a duration as fractional days truncated to 2
// decimal places via the JSON encoder's float64 default rounding.
// Negative or zero stays as 0.
func daysFromDuration(d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return d.Hours() / 24.0
}

// wipe zeros a byte slice via the same constant-time copy idiom used by
// the auth-side DEK cache (US-001). The plaintext DEK lives in this
// function's stack frame after GenerateDataKey returns; zeroing it on
// the way out shrinks the post-handler heap exposure window.
func wipe(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
