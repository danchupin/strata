package s3api

import (
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/danchupin/strata/internal/meta"
)

type aclGrantee struct {
	XMLName     xml.Name `xml:"Grantee"`
	XSI         string   `xml:"xmlns:xsi,attr,omitempty"`
	Type        string   `xml:"http://www.w3.org/2001/XMLSchema-instance type,attr"`
	ID          string   `xml:"ID,omitempty"`
	DisplayName string   `xml:"DisplayName,omitempty"`
	URI         string   `xml:"URI,omitempty"`
	Email       string   `xml:"EmailAddress,omitempty"`
}

type aclGrant struct {
	XMLName    xml.Name   `xml:"Grant"`
	Grantee    aclGrantee `xml:"Grantee"`
	Permission string     `xml:"Permission"`
}

type accessControlPolicy struct {
	XMLName           xml.Name `xml:"AccessControlPolicy"`
	Owner             owner    `xml:"Owner"`
	AccessControlList struct {
		Grants []aclGrant `xml:"Grant"`
	} `xml:"AccessControlList"`
}

const (
	cannedPrivate           = "private"
	cannedPublicRead        = "public-read"
	cannedPublicReadWrite   = "public-read-write"
	cannedAuthenticatedRead = "authenticated-read"
)

const (
	groupAllUsers           = "http://acs.amazonaws.com/groups/global/AllUsers"
	groupAuthenticatedUsers = "http://acs.amazonaws.com/groups/global/AuthenticatedUsers"
	groupLogDelivery        = "http://acs.amazonaws.com/groups/s3/LogDelivery"
)

var (
	allowedGranteeTypes = map[string]struct{}{
		"CanonicalUser":          {},
		"Group":                  {},
		"AmazonCustomerByEmail":  {},
	}
	allowedPermissions = map[string]struct{}{
		"FULL_CONTROL": {},
		"READ":         {},
		"WRITE":        {},
		"READ_ACP":     {},
		"WRITE_ACP":    {},
	}
	allowedGroupURIs = map[string]struct{}{
		groupAllUsers:           {},
		groupAuthenticatedUsers: {},
		groupLogDelivery:        {},
	}
)

func normalizeCannedACL(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", cannedPrivate:
		return cannedPrivate
	case cannedPublicRead:
		return cannedPublicRead
	case cannedPublicReadWrite:
		return cannedPublicReadWrite
	case cannedAuthenticatedRead:
		return cannedAuthenticatedRead
	}
	return cannedPrivate
}

func buildACL(canned, ownerID string) accessControlPolicy {
	ownerID = firstNonEmpty(ownerID, "strata")
	policy := accessControlPolicy{
		Owner: owner{ID: ownerID, DisplayName: ownerID},
	}
	policy.AccessControlList.Grants = append(policy.AccessControlList.Grants, aclGrant{
		Grantee: aclGrantee{
			XSI:         "http://www.w3.org/2001/XMLSchema-instance",
			Type:        "CanonicalUser",
			ID:          ownerID,
			DisplayName: ownerID,
		},
		Permission: "FULL_CONTROL",
	})
	switch canned {
	case cannedPublicRead:
		policy.AccessControlList.Grants = append(policy.AccessControlList.Grants, aclGrant{
			Grantee:    aclGrantee{XSI: "http://www.w3.org/2001/XMLSchema-instance", Type: "Group", URI: groupAllUsers},
			Permission: "READ",
		})
	case cannedPublicReadWrite:
		policy.AccessControlList.Grants = append(policy.AccessControlList.Grants,
			aclGrant{Grantee: aclGrantee{XSI: "http://www.w3.org/2001/XMLSchema-instance", Type: "Group", URI: groupAllUsers}, Permission: "READ"},
			aclGrant{Grantee: aclGrantee{XSI: "http://www.w3.org/2001/XMLSchema-instance", Type: "Group", URI: groupAllUsers}, Permission: "WRITE"},
		)
	case cannedAuthenticatedRead:
		policy.AccessControlList.Grants = append(policy.AccessControlList.Grants, aclGrant{
			Grantee:    aclGrantee{XSI: "http://www.w3.org/2001/XMLSchema-instance", Type: "Group", URI: groupAuthenticatedUsers},
			Permission: "READ",
		})
	}
	return policy
}

func buildACLFromGrants(ownerID string, grants []meta.Grant) accessControlPolicy {
	ownerID = firstNonEmpty(ownerID, "strata")
	policy := accessControlPolicy{
		Owner: owner{ID: ownerID, DisplayName: ownerID},
	}
	for _, g := range grants {
		policy.AccessControlList.Grants = append(policy.AccessControlList.Grants, aclGrant{
			Grantee: aclGrantee{
				XSI:         "http://www.w3.org/2001/XMLSchema-instance",
				Type:        g.GranteeType,
				ID:          g.ID,
				DisplayName: g.DisplayName,
				URI:         g.URI,
				Email:       g.Email,
			},
			Permission: g.Permission,
		})
	}
	return policy
}

// parseACLBody reads and validates an AccessControlPolicy request body.
// Returns (nil, false, nil) when no body is present.
func parseACLBody(r *http.Request) (grants []meta.Grant, hadBody bool, err error) {
	body, readErr := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if readErr != nil {
		return nil, false, readErr
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil, false, nil
	}
	var policy accessControlPolicy
	if uerr := xml.Unmarshal(body, &policy); uerr != nil {
		return nil, true, errMalformedACL
	}
	out := make([]meta.Grant, 0, len(policy.AccessControlList.Grants))
	for _, g := range policy.AccessControlList.Grants {
		gt := strings.TrimSpace(g.Grantee.Type)
		if gt == "" {
			return nil, true, errMalformedACL
		}
		if _, ok := allowedGranteeTypes[gt]; !ok {
			return nil, true, errMalformedACL
		}
		perm := strings.TrimSpace(g.Permission)
		if _, ok := allowedPermissions[perm]; !ok {
			return nil, true, errMalformedACL
		}
		switch gt {
		case "CanonicalUser":
			if g.Grantee.ID == "" {
				return nil, true, errMalformedACL
			}
		case "Group":
			if _, ok := allowedGroupURIs[g.Grantee.URI]; !ok {
				return nil, true, errMalformedACL
			}
		case "AmazonCustomerByEmail":
			if g.Grantee.Email == "" {
				return nil, true, errMalformedACL
			}
		}
		out = append(out, meta.Grant{
			GranteeType: gt,
			ID:          g.Grantee.ID,
			URI:         g.Grantee.URI,
			DisplayName: g.Grantee.DisplayName,
			Email:       g.Grantee.Email,
			Permission:  perm,
		})
	}
	return out, true, nil
}

var errMalformedACL = errors.New("malformed acl")

func (s *Server) getBucketACL(w http.ResponseWriter, r *http.Request, bucket string) {
	b, err := s.Meta.GetBucket(r.Context(), bucket)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	if grants, gerr := s.Meta.GetBucketGrants(r.Context(), b.ID); gerr == nil {
		writeXML(w, http.StatusOK, buildACLFromGrants(b.Owner, grants))
		return
	}
	canned := firstNonEmpty(b.ACL, cannedPrivate)
	writeXML(w, http.StatusOK, buildACL(canned, b.Owner))
}

func (s *Server) putBucketACL(w http.ResponseWriter, r *http.Request, bucket string) {
	b, err := s.Meta.GetBucket(r.Context(), bucket)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	grants, hadBody, perr := parseACLBody(r)
	if errors.Is(perr, errMalformedACL) {
		writeError(w, r, ErrMalformedACLError)
		return
	}
	if perr != nil {
		writeError(w, r, ErrInternal)
		return
	}
	if aclHdr := r.Header.Get("x-amz-acl"); aclHdr != "" {
		if err := s.Meta.SetBucketACL(r.Context(), b.Name, normalizeCannedACL(aclHdr)); err != nil {
			mapMetaErr(w, r, err)
			return
		}
	}
	if hadBody {
		if err := s.Meta.SetBucketGrants(r.Context(), b.ID, grants); err != nil {
			mapMetaErr(w, r, err)
			return
		}
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) getObjectACL(w http.ResponseWriter, r *http.Request, b *meta.Bucket, key string) {
	versionID := r.URL.Query().Get("versionId")
	o, err := s.Meta.GetObject(r.Context(), b.ID, key, versionID)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	if o.VersionID != "" {
		w.Header().Set("x-amz-version-id", wireVersionID(o))
	}
	if grants, gerr := s.Meta.GetObjectGrants(r.Context(), b.ID, key, o.VersionID); gerr == nil {
		writeXML(w, http.StatusOK, buildACLFromGrants(b.Owner, grants))
		return
	}
	writeXML(w, http.StatusOK, buildACL(cannedPrivate, b.Owner))
}

func (s *Server) putObjectACL(w http.ResponseWriter, r *http.Request, b *meta.Bucket, key string) {
	versionID := r.URL.Query().Get("versionId")
	o, err := s.Meta.GetObject(r.Context(), b.ID, key, versionID)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	grants, hadBody, perr := parseACLBody(r)
	if errors.Is(perr, errMalformedACL) {
		writeError(w, r, ErrMalformedACLError)
		return
	}
	if perr != nil {
		writeError(w, r, ErrInternal)
		return
	}
	if hadBody {
		if err := s.Meta.SetObjectGrants(r.Context(), b.ID, key, o.VersionID, grants); err != nil {
			mapMetaErr(w, r, err)
			return
		}
	}
	w.WriteHeader(http.StatusOK)
}
