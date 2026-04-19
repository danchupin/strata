package s3api

import (
	"encoding/xml"
	"net/http"
	"strings"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/meta"
)

type aclGrantee struct {
	XMLName     xml.Name `xml:"Grantee"`
	XSI         string   `xml:"xmlns:xsi,attr,omitempty"`
	Type        string   `xml:"xsi:type,attr"`
	ID          string   `xml:"ID,omitempty"`
	DisplayName string   `xml:"DisplayName,omitempty"`
	URI         string   `xml:"URI,omitempty"`
}

type aclGrant struct {
	XMLName    xml.Name   `xml:"Grant"`
	Grantee    aclGrantee `xml:"Grantee"`
	Permission string     `xml:"Permission"`
}

type accessControlPolicy struct {
	XMLName           xml.Name   `xml:"AccessControlPolicy"`
	Owner             owner      `xml:"Owner"`
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

func (s *Server) getBucketACL(w http.ResponseWriter, r *http.Request, bucket string) {
	b, err := s.Meta.GetBucket(r.Context(), bucket)
	if err != nil {
		mapMetaErr(w, r, err)
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
	canned := normalizeCannedACL(r.Header.Get("x-amz-acl"))
	if err := s.Meta.SetBucketACL(r.Context(), b.Name, canned); err != nil {
		mapMetaErr(w, r, err)
		return
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
		w.Header().Set("x-amz-version-id", o.VersionID)
	}
	writeXML(w, http.StatusOK, buildACL(cannedPrivate, b.Owner))
}

func (s *Server) putObjectACL(w http.ResponseWriter, r *http.Request, b *meta.Bucket, key string) {
	versionID := r.URL.Query().Get("versionId")
	if _, err := s.Meta.GetObject(r.Context(), b.ID, key, versionID); err != nil {
		mapMetaErr(w, r, err)
		return
	}
	_ = auth.FromContext(r.Context())
	w.WriteHeader(http.StatusOK)
}
