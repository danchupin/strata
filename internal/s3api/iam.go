package s3api

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/meta"
)

var base64Std = base64.StdEncoding

// IAMRootPrincipal is the canonical owner string for the hardcoded [iam root]
// identity. Requests to ?Action= endpoints must carry an authenticated context
// whose Owner matches this constant.
const IAMRootPrincipal = "iam-root"

const iamXMLNS = "https://iam.amazonaws.com/doc/2010-05-08/"

var (
	errIAMEntityAlreadyExists = APIError{Code: "EntityAlreadyExists", Message: "The request was rejected because it attempted to create a resource that already exists.", Status: http.StatusConflict}
	errIAMNoSuchEntity        = APIError{Code: "NoSuchEntity", Message: "The request was rejected because it referenced an entity that does not exist.", Status: http.StatusNotFound}
	errIAMValidation          = APIError{Code: "ValidationError", Message: "The request was rejected because a parameter is invalid.", Status: http.StatusBadRequest}
	errIAMUnsupportedAction   = APIError{Code: "InvalidAction", Message: "The action is not supported.", Status: http.StatusBadRequest}
)

func extractIAMAction(r *http.Request) string {
	if a := r.URL.Query().Get("Action"); a != "" {
		return a
	}
	if r.Method == http.MethodPost {
		ct := r.Header.Get("Content-Type")
		if strings.HasPrefix(ct, "application/x-www-form-urlencoded") {
			if err := r.ParseForm(); err == nil {
				if a := r.PostForm.Get("Action"); a != "" {
					return a
				}
			}
		}
	}
	return ""
}

func iamParam(r *http.Request, key string) string {
	if v := r.URL.Query().Get(key); v != "" {
		return v
	}
	if r.PostForm != nil {
		if v := r.PostForm.Get(key); v != "" {
			return v
		}
	}
	return ""
}

func (s *Server) handleIAM(w http.ResponseWriter, r *http.Request, action string) {
	info := auth.FromContext(r.Context())
	if info == nil || info.IsAnonymous || info.Owner != IAMRootPrincipal {
		writeError(w, r, ErrAccessDenied)
		return
	}
	switch action {
	case "CreateUser":
		s.iamCreateUser(w, r)
	case "GetUser":
		s.iamGetUser(w, r)
	case "ListUsers":
		s.iamListUsers(w, r)
	case "DeleteUser":
		s.iamDeleteUser(w, r)
	case "CreateAccessKey":
		s.iamCreateAccessKey(w, r)
	case "ListAccessKeys":
		s.iamListAccessKeys(w, r)
	case "DeleteAccessKey":
		s.iamDeleteAccessKey(w, r)
	case "AssumeRole":
		s.stsAssumeRole(w, r)
	default:
		writeError(w, r, errIAMUnsupportedAction)
	}
}

type iamUser struct {
	Path       string `xml:"Path"`
	UserName   string `xml:"UserName"`
	UserID     string `xml:"UserId"`
	Arn        string `xml:"Arn"`
	CreateDate string `xml:"CreateDate"`
}

type iamResponseMetadata struct {
	RequestID string `xml:"RequestId"`
}

type createUserResponse struct {
	XMLName  xml.Name            `xml:"CreateUserResponse"`
	XMLNS    string              `xml:"xmlns,attr"`
	Result   createUserResult    `xml:"CreateUserResult"`
	Metadata iamResponseMetadata `xml:"ResponseMetadata"`
}

type createUserResult struct {
	User iamUser `xml:"User"`
}

type getUserResponse struct {
	XMLName  xml.Name            `xml:"GetUserResponse"`
	XMLNS    string              `xml:"xmlns,attr"`
	Result   getUserResult       `xml:"GetUserResult"`
	Metadata iamResponseMetadata `xml:"ResponseMetadata"`
}

type getUserResult struct {
	User iamUser `xml:"User"`
}

type listUsersResponse struct {
	XMLName  xml.Name            `xml:"ListUsersResponse"`
	XMLNS    string              `xml:"xmlns,attr"`
	Result   listUsersResult     `xml:"ListUsersResult"`
	Metadata iamResponseMetadata `xml:"ResponseMetadata"`
}

type listUsersResult struct {
	Users       iamUserList `xml:"Users"`
	IsTruncated bool        `xml:"IsTruncated"`
}

type iamUserList struct {
	Members []iamUser `xml:"member"`
}

type deleteUserResponse struct {
	XMLName  xml.Name            `xml:"DeleteUserResponse"`
	XMLNS    string              `xml:"xmlns,attr"`
	Metadata iamResponseMetadata `xml:"ResponseMetadata"`
}

func newRequestID() string {
	var buf [16]byte
	_, _ = rand.Read(buf[:])
	return hex.EncodeToString(buf[:])
}

func newUserID() string {
	var buf [10]byte
	_, _ = rand.Read(buf[:])
	return "AID" + strings.ToUpper(hex.EncodeToString(buf[:]))
}

func userArn(path, name string) string {
	if path == "" || !strings.HasPrefix(path, "/") {
		path = "/"
	}
	return "arn:aws:iam::strata:user" + path + name
}

func iamUserView(u *meta.IAMUser) iamUser {
	path := u.Path
	if path == "" {
		path = "/"
	}
	return iamUser{
		Path:       path,
		UserName:   u.UserName,
		UserID:     u.UserID,
		Arn:        userArn(path, u.UserName),
		CreateDate: u.CreatedAt.UTC().Format(time.RFC3339),
	}
}

func (s *Server) iamCreateUser(w http.ResponseWriter, r *http.Request) {
	name := iamParam(r, "UserName")
	if name == "" {
		writeError(w, r, errIAMValidation)
		return
	}
	path := iamParam(r, "Path")
	if path == "" {
		path = "/"
	}
	u := &meta.IAMUser{
		UserName:  name,
		UserID:    newUserID(),
		Path:      path,
		CreatedAt: time.Now().UTC(),
	}
	if err := s.Meta.CreateIAMUser(r.Context(), u); err != nil {
		if errors.Is(err, meta.ErrIAMUserAlreadyExists) {
			writeError(w, r, errIAMEntityAlreadyExists)
			return
		}
		writeError(w, r, ErrInternal)
		return
	}
	writeXML(w, http.StatusOK, createUserResponse{
		XMLNS:    iamXMLNS,
		Result:   createUserResult{User: iamUserView(u)},
		Metadata: iamResponseMetadata{RequestID: newRequestID()},
	})
}

func (s *Server) iamGetUser(w http.ResponseWriter, r *http.Request) {
	name := iamParam(r, "UserName")
	if name == "" {
		writeError(w, r, errIAMValidation)
		return
	}
	u, err := s.Meta.GetIAMUser(r.Context(), name)
	if err != nil {
		if errors.Is(err, meta.ErrIAMUserNotFound) {
			writeError(w, r, errIAMNoSuchEntity)
			return
		}
		writeError(w, r, ErrInternal)
		return
	}
	writeXML(w, http.StatusOK, getUserResponse{
		XMLNS:    iamXMLNS,
		Result:   getUserResult{User: iamUserView(u)},
		Metadata: iamResponseMetadata{RequestID: newRequestID()},
	})
}

func (s *Server) iamListUsers(w http.ResponseWriter, r *http.Request) {
	prefix := iamParam(r, "PathPrefix")
	users, err := s.Meta.ListIAMUsers(r.Context(), prefix)
	if err != nil {
		writeError(w, r, ErrInternal)
		return
	}
	out := listUsersResult{}
	for _, u := range users {
		out.Users.Members = append(out.Users.Members, iamUserView(u))
	}
	writeXML(w, http.StatusOK, listUsersResponse{
		XMLNS:    iamXMLNS,
		Result:   out,
		Metadata: iamResponseMetadata{RequestID: newRequestID()},
	})
}

type iamAccessKey struct {
	UserName        string `xml:"UserName"`
	AccessKeyID     string `xml:"AccessKeyId"`
	Status          string `xml:"Status"`
	SecretAccessKey string `xml:"SecretAccessKey,omitempty"`
	CreateDate      string `xml:"CreateDate"`
}

type iamAccessKeyMeta struct {
	UserName    string `xml:"UserName"`
	AccessKeyID string `xml:"AccessKeyId"`
	Status      string `xml:"Status"`
	CreateDate  string `xml:"CreateDate"`
}

type createAccessKeyResponse struct {
	XMLName  xml.Name              `xml:"CreateAccessKeyResponse"`
	XMLNS    string                `xml:"xmlns,attr"`
	Result   createAccessKeyResult `xml:"CreateAccessKeyResult"`
	Metadata iamResponseMetadata   `xml:"ResponseMetadata"`
}

type createAccessKeyResult struct {
	AccessKey iamAccessKey `xml:"AccessKey"`
}

type listAccessKeysResponse struct {
	XMLName  xml.Name             `xml:"ListAccessKeysResponse"`
	XMLNS    string               `xml:"xmlns,attr"`
	Result   listAccessKeysResult `xml:"ListAccessKeysResult"`
	Metadata iamResponseMetadata  `xml:"ResponseMetadata"`
}

type listAccessKeysResult struct {
	UserName          string                  `xml:"UserName"`
	AccessKeyMetadata listAccessKeysMemberSet `xml:"AccessKeyMetadata"`
	IsTruncated       bool                    `xml:"IsTruncated"`
}

type listAccessKeysMemberSet struct {
	Members []iamAccessKeyMeta `xml:"member"`
}

type deleteAccessKeyResponse struct {
	XMLName  xml.Name            `xml:"DeleteAccessKeyResponse"`
	XMLNS    string              `xml:"xmlns,attr"`
	Metadata iamResponseMetadata `xml:"ResponseMetadata"`
}

func newAccessKeyID() string {
	var buf [10]byte
	_, _ = rand.Read(buf[:])
	return "AKIA" + strings.ToUpper(hex.EncodeToString(buf[:]))
}

func newSecretAccessKey() string {
	var buf [30]byte
	_, _ = rand.Read(buf[:])
	return base64Std.EncodeToString(buf[:])
}

func accessKeyStatus(disabled bool) string {
	if disabled {
		return "Inactive"
	}
	return "Active"
}

func (s *Server) iamCreateAccessKey(w http.ResponseWriter, r *http.Request) {
	userName := iamParam(r, "UserName")
	if userName == "" {
		writeError(w, r, errIAMValidation)
		return
	}
	if _, err := s.Meta.GetIAMUser(r.Context(), userName); err != nil {
		if errors.Is(err, meta.ErrIAMUserNotFound) {
			writeError(w, r, errIAMNoSuchEntity)
			return
		}
		writeError(w, r, ErrInternal)
		return
	}
	ak := &meta.IAMAccessKey{
		AccessKeyID:     newAccessKeyID(),
		SecretAccessKey: newSecretAccessKey(),
		UserName:        userName,
		CreatedAt:       time.Now().UTC(),
	}
	if err := s.Meta.CreateIAMAccessKey(r.Context(), ak); err != nil {
		writeError(w, r, ErrInternal)
		return
	}
	writeXML(w, http.StatusOK, createAccessKeyResponse{
		XMLNS: iamXMLNS,
		Result: createAccessKeyResult{AccessKey: iamAccessKey{
			UserName:        ak.UserName,
			AccessKeyID:     ak.AccessKeyID,
			Status:          accessKeyStatus(ak.Disabled),
			SecretAccessKey: ak.SecretAccessKey,
			CreateDate:      ak.CreatedAt.UTC().Format(time.RFC3339),
		}},
		Metadata: iamResponseMetadata{RequestID: newRequestID()},
	})
}

func (s *Server) iamListAccessKeys(w http.ResponseWriter, r *http.Request) {
	userName := iamParam(r, "UserName")
	if userName == "" {
		writeError(w, r, errIAMValidation)
		return
	}
	if _, err := s.Meta.GetIAMUser(r.Context(), userName); err != nil {
		if errors.Is(err, meta.ErrIAMUserNotFound) {
			writeError(w, r, errIAMNoSuchEntity)
			return
		}
		writeError(w, r, ErrInternal)
		return
	}
	keys, err := s.Meta.ListIAMAccessKeys(r.Context(), userName)
	if err != nil {
		writeError(w, r, ErrInternal)
		return
	}
	out := listAccessKeysResult{UserName: userName}
	for _, ak := range keys {
		out.AccessKeyMetadata.Members = append(out.AccessKeyMetadata.Members, iamAccessKeyMeta{
			UserName:    ak.UserName,
			AccessKeyID: ak.AccessKeyID,
			Status:      accessKeyStatus(ak.Disabled),
			CreateDate:  ak.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	writeXML(w, http.StatusOK, listAccessKeysResponse{
		XMLNS:    iamXMLNS,
		Result:   out,
		Metadata: iamResponseMetadata{RequestID: newRequestID()},
	})
}

func (s *Server) iamDeleteAccessKey(w http.ResponseWriter, r *http.Request) {
	accessKeyID := iamParam(r, "AccessKeyId")
	if accessKeyID == "" {
		writeError(w, r, errIAMValidation)
		return
	}
	if _, err := s.Meta.DeleteIAMAccessKey(r.Context(), accessKeyID); err != nil {
		if errors.Is(err, meta.ErrIAMAccessKeyNotFound) {
			writeError(w, r, errIAMNoSuchEntity)
			return
		}
		writeError(w, r, ErrInternal)
		return
	}
	if s.InvalidateCredential != nil {
		s.InvalidateCredential(accessKeyID)
	}
	writeXML(w, http.StatusOK, deleteAccessKeyResponse{
		XMLNS:    iamXMLNS,
		Metadata: iamResponseMetadata{RequestID: newRequestID()},
	})
}

func (s *Server) iamDeleteUser(w http.ResponseWriter, r *http.Request) {
	name := iamParam(r, "UserName")
	if name == "" {
		writeError(w, r, errIAMValidation)
		return
	}
	if err := s.Meta.DeleteIAMUser(r.Context(), name); err != nil {
		if errors.Is(err, meta.ErrIAMUserNotFound) {
			writeError(w, r, errIAMNoSuchEntity)
			return
		}
		writeError(w, r, ErrInternal)
		return
	}
	writeXML(w, http.StatusOK, deleteUserResponse{
		XMLNS:    iamXMLNS,
		Metadata: iamResponseMetadata{RequestID: newRequestID()},
	})
}
