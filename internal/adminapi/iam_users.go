package adminapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/leader"
	"github.com/danchupin/strata/internal/logging"
	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/s3api"
)

// iamUserNamePattern enforces the AWS IAM user-name charset (1..64 chars,
// alphanumerics + plus / equals / comma / period / at / dash / underscore).
var iamUserNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_+=,.@-]{1,64}$`)

// iamUserDeleteLockTTL bounds the cascade delete (drop access keys, drop
// user) — a single user rarely owns more than a handful of keys, so this is
// generous and only matters if the meta backend stalls mid-flight.
const iamUserDeleteLockTTL = 2 * time.Minute

// iamUserDeleteLockName returns the worker_locks key reserved for the
// per-user cascade delete (US-011).
func iamUserDeleteLockName(userName string) string {
	return "iam-user:" + userName
}

// IAMUserSummary is the wire shape for one entry in the users list. Counts
// the access keys owned by the user so the table can render the column
// without a second round-trip.
type IAMUserSummary struct {
	UserName       string `json:"user_name"`
	UserID         string `json:"user_id"`
	Path           string `json:"path"`
	CreatedAt      int64  `json:"created_at"`
	AccessKeyCount int    `json:"access_key_count"`
}

// IAMUsersListResponse is the response shape for GET /admin/v1/iam/users.
// Total is the matching-row count BEFORE pagination so the UI can render a
// page-count chip identical to the buckets-list page.
type IAMUsersListResponse struct {
	Users []IAMUserSummary `json:"users"`
	Total int              `json:"total"`
}

// CreateIAMUserRequest is the JSON body accepted by POST /admin/v1/iam/users.
// Path defaults to "/" when empty (AWS IAM convention).
type CreateIAMUserRequest struct {
	UserName string `json:"user_name"`
	Path     string `json:"path"`
}

// IAMUserDeletePreviewResponse is the wire shape carried inside a 409
// IAMUserHasAccessKeys body so the UI can render the keys that will be
// cascade-deleted.
type IAMUserDeletePreviewResponse struct {
	UserName        string   `json:"user_name"`
	AccessKeyIDs    []string `json:"access_key_ids"`
	AccessKeyCount  int      `json:"access_key_count"`
}

// handleIAMUsersList serves GET /admin/v1/iam/users.
//
// Query params: query (case-insensitive substring on UserName), page (1-based,
// default 1), page_size (1..500, default 50). Returns IAMUsersListResponse;
// AccessKeyCount is computed via meta.Store.ListIAMAccessKeys per row.
func (s *Server) handleIAMUsersList(w http.ResponseWriter, r *http.Request) {
	if s.Meta == nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", "meta store not configured")
		return
	}
	q := r.URL.Query()
	queryLower := strings.ToLower(strings.TrimSpace(q.Get("query")))
	page := parsePositive(q.Get("page"), 1)
	pageSize := parseRange(q.Get("page_size"), 50, 1, 500)

	users, err := s.Meta.ListIAMUsers(r.Context(), "")
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}

	matched := make([]*meta.IAMUser, 0, len(users))
	for _, u := range users {
		if queryLower != "" && !strings.Contains(strings.ToLower(u.UserName), queryLower) {
			continue
		}
		matched = append(matched, u)
	}

	total := len(matched)
	start := (page - 1) * pageSize
	if start > total {
		start = total
	}
	end := start + pageSize
	if end > total {
		end = total
	}
	pageRows := matched[start:end]

	out := IAMUsersListResponse{
		Users: make([]IAMUserSummary, 0, len(pageRows)),
		Total: total,
	}
	for _, u := range pageRows {
		count := 0
		keys, kerr := s.Meta.ListIAMAccessKeys(r.Context(), u.UserName)
		if kerr == nil {
			count = len(keys)
		}
		out.Users = append(out.Users, IAMUserSummary{
			UserName:       u.UserName,
			UserID:         u.UserID,
			Path:           u.Path,
			CreatedAt:      u.CreatedAt.Unix(),
			AccessKeyCount: count,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleIAMUserCreate serves POST /admin/v1/iam/users (US-011). Mints a
// fresh meta.IAMUser record. Audit row admin:CreateUser, resource
// iam:<userName>.
func (s *Server) handleIAMUserCreate(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	if s.Meta == nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", "meta store not configured")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<10))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "failed to read body")
		return
	}
	var req CreateIAMUserRequest
	if jerr := json.Unmarshal(body, &req); jerr != nil {
		writeJSONError(w, http.StatusBadRequest, "MalformedRequest", "invalid JSON: "+jerr.Error())
		return
	}
	name := strings.TrimSpace(req.UserName)
	if !iamUserNamePattern.MatchString(name) {
		writeJSONError(w, http.StatusBadRequest, "InvalidUserName",
			"user_name must match ^[a-zA-Z0-9_+=,.@-]{1,64}$")
		return
	}
	path := strings.TrimSpace(req.Path)
	if path == "" {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") || !strings.HasSuffix(path, "/") {
		writeJSONError(w, http.StatusBadRequest, "InvalidPath",
			"path must begin and end with '/'")
		return
	}

	ctx := r.Context()
	owner := auth.FromContext(ctx).Owner
	s3api.SetAuditOverride(ctx, "admin:CreateUser", "iam:"+name, "", owner)

	u := &meta.IAMUser{
		UserName:  name,
		UserID:    "AID" + strings.ToUpper(strings.ReplaceAll(uuid.NewString(), "-", "")),
		Path:      path,
		CreatedAt: time.Now().UTC(),
	}
	if cerr := s.Meta.CreateIAMUser(ctx, u); cerr != nil {
		if errors.Is(cerr, meta.ErrIAMUserAlreadyExists) {
			writeJSONError(w, http.StatusConflict, "EntityAlreadyExists",
				fmt.Sprintf("user %q already exists", name))
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", cerr.Error())
		return
	}
	writeJSON(w, http.StatusCreated, IAMUserSummary{
		UserName:       u.UserName,
		UserID:         u.UserID,
		Path:           u.Path,
		CreatedAt:      u.CreatedAt.Unix(),
		AccessKeyCount: 0,
	})
}

// handleIAMUserDelete serves DELETE /admin/v1/iam/users/{userName} (US-011).
// Cascade: drops every access key the user owns (one DeleteIAMAccessKey per
// key), then DeleteIAMUser. Wraps the cascade in a `iam-user:<name>` lease
// against worker_locks so concurrent deletes serialise instead of racing.
//
// Audit:
//   - admin:DeleteUser stamped via SetAuditOverride for the request itself
//   - admin:DeleteAccessKey emitted via meta.Store.EnqueueAudit for each
//     cascaded key (one row per key)
func (s *Server) handleIAMUserDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("userName")
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "user_name is required")
		return
	}
	if s.Meta == nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", "meta store not configured")
		return
	}
	if s.Locker == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "LockerUnavailable",
			"meta backend exposes no leader-election locker; user delete unavailable")
		return
	}
	ctx := r.Context()
	owner := auth.FromContext(ctx).Owner
	s3api.SetAuditOverride(ctx, "admin:DeleteUser", "iam:"+name, "", owner)

	lockName := iamUserDeleteLockName(name)
	holder := leader.DefaultHolder()
	acquired, err := s.Locker.Acquire(ctx, lockName, holder, iamUserDeleteLockTTL)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal",
			fmt.Sprintf("acquire lock: %v", err))
		return
	}
	if !acquired {
		writeJSONError(w, http.StatusConflict, "DeleteUserInProgress",
			"another delete is already in flight for this user")
		return
	}
	defer func() {
		if rerr := s.Locker.Release(context.Background(), lockName, holder); rerr != nil {
			s.Logger.Printf("adminapi: iam-user release lease %q: %v", lockName, rerr)
		}
	}()

	if _, gerr := s.Meta.GetIAMUser(ctx, name); gerr != nil {
		if errors.Is(gerr, meta.ErrIAMUserNotFound) {
			writeJSONError(w, http.StatusNotFound, "NoSuchEntity", "user not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", gerr.Error())
		return
	}

	keys, kerr := s.Meta.ListIAMAccessKeys(ctx, name)
	if kerr != nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal",
			fmt.Sprintf("list access keys: %v", kerr))
		return
	}
	for _, ak := range keys {
		if _, derr := s.Meta.DeleteIAMAccessKey(ctx, ak.AccessKeyID); derr != nil {
			if errors.Is(derr, meta.ErrIAMAccessKeyNotFound) {
				continue
			}
			writeJSONError(w, http.StatusInternalServerError, "Internal",
				fmt.Sprintf("cascade delete access key %q: %v", ak.AccessKeyID, derr))
			return
		}
		s.emitAuditRow(ctx, r, &meta.AuditEvent{
			Time:      time.Now().UTC(),
			Principal: owner,
			Action:    "admin:DeleteAccessKey",
			Resource:  "iam-access-key:" + ak.AccessKeyID,
			Result:    strconv.Itoa(http.StatusNoContent),
		})
	}

	if derr := s.Meta.DeleteIAMUser(ctx, name); derr != nil {
		if errors.Is(derr, meta.ErrIAMUserNotFound) {
			writeJSONError(w, http.StatusNotFound, "NoSuchEntity", "user not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", derr.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// emitAuditRow appends a best-effort audit_log row using the configured
// AuditTTL. Used by admin handlers that need to record more than one audit
// entry per request (e.g. cascaded deletes). Falls back to
// s3api.DefaultAuditRetention when AuditTTL is zero.
func (s *Server) emitAuditRow(ctx context.Context, r *http.Request, ev *meta.AuditEvent) {
	if s.Meta == nil || ev == nil {
		return
	}
	if ev.Bucket == "" {
		ev.Bucket = "-"
	}
	if ev.RequestID == "" {
		ev.RequestID = logging.RequestIDFromContext(ctx)
	}
	if ev.RequestID == "" && r != nil {
		ev.RequestID = r.Header.Get(logging.HeaderRequestID)
	}
	ttl := s.AuditTTL
	if ttl == 0 {
		ttl = s3api.DefaultAuditRetention
	}
	_ = s.Meta.EnqueueAudit(ctx, ev, ttl)
}
