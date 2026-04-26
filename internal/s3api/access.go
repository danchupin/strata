package s3api

import (
	"errors"
	"net/http"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/auth/policy"
	"github.com/danchupin/strata/internal/meta"
)

func principalForRequest(r *http.Request) string {
	info := auth.FromContext(r.Context())
	if info == nil || info.IsAnonymous {
		return "*"
	}
	if info.Owner == "" {
		return "*"
	}
	return info.Owner
}

func bucketARN(name string) string        { return "arn:aws:s3:::" + name }
func objectARN(bucket, key string) string { return "arn:aws:s3:::" + bucket + "/" + key }

func (s *Server) loadBucketPolicy(r *http.Request, b *meta.Bucket) (*policy.Document, error) {
	blob, err := s.Meta.GetBucketPolicy(r.Context(), b.ID)
	if err != nil {
		if errors.Is(err, meta.ErrNoSuchBucketPolicy) {
			return nil, nil
		}
		return nil, err
	}
	if len(blob) == 0 {
		return nil, nil
	}
	return policy.Parse(blob)
}

// requireAccess gates a request against the bucket policy. When no policy is
// set, the request proceeds. Otherwise the request is allowed only if a
// statement matches with Effect=Allow and no matching statement uses Deny.
func (s *Server) requireAccess(w http.ResponseWriter, r *http.Request, b *meta.Bucket, action, resourceARN string) bool {
	doc, err := s.loadBucketPolicy(r, b)
	if err != nil {
		writeError(w, r, ErrInternal)
		return false
	}
	if doc == nil {
		return true
	}
	decision, err := policy.Evaluate(doc, principalForRequest(r), action, resourceARN, nil)
	if err != nil {
		writeError(w, r, ErrInternal)
		return false
	}
	if decision == policy.Allow {
		return true
	}
	writeError(w, r, ErrAccessDenied)
	return false
}

// requireObjectAccess runs the policy gate then the ACL gate for an object
// action. Both gates must pass; an explicit policy Deny short-circuits.
func (s *Server) requireObjectAccess(w http.ResponseWriter, r *http.Request, b *meta.Bucket, key, action string) bool {
	if !s.requireAccess(w, r, b, action, objectARN(b.Name, key)) {
		return false
	}
	return s.requireACL(w, r, b, key, action)
}

// requireACL enforces canned bucket ACLs and persisted bucket/object grants
// for object-level requests. The bucket owner always has FULL_CONTROL.
// Per-object grants override bucket ACL on read actions; bucket-level
// persisted grants take priority over the canned ACL otherwise.
func (s *Server) requireACL(w http.ResponseWriter, r *http.Request, b *meta.Bucket, key, action string) bool {
	info := auth.FromContext(r.Context())
	if info != nil && info.Owner != "" && info.Owner == b.Owner {
		return true
	}
	want := requiredPermForAction(action)

	if action == "s3:GetObject" && key != "" {
		if grants, err := s.Meta.GetObjectGrants(r.Context(), b.ID, key, ""); err == nil {
			if grantsAllow(grants, info, want) {
				return true
			}
			writeError(w, r, ErrAccessDenied)
			return false
		}
	}

	if grants, err := s.Meta.GetBucketGrants(r.Context(), b.ID); err == nil {
		if grantsAllow(grants, info, want) {
			return true
		}
		writeError(w, r, ErrAccessDenied)
		return false
	}

	if cannedAllows(b.ACL, action, info) {
		return true
	}
	writeError(w, r, ErrAccessDenied)
	return false
}

func requiredPermForAction(action string) string {
	switch action {
	case "s3:GetObject", "s3:ListBucket", "s3:ListBucketVersions", "s3:ListMultipartUploadParts":
		return "READ"
	case "s3:PutObject", "s3:DeleteObject", "s3:DeleteObjectVersion", "s3:AbortMultipartUpload":
		return "WRITE"
	}
	return ""
}

func cannedAllows(canned, action string, info *auth.AuthInfo) bool {
	switch canned {
	case cannedPublicRead:
		return action == "s3:GetObject" || action == "s3:ListBucket" || action == "s3:ListBucketVersions" || action == "s3:ListMultipartUploadParts"
	case cannedPublicReadWrite:
		return true
	case cannedAuthenticatedRead:
		if info == nil || info.IsAnonymous {
			return false
		}
		return action == "s3:GetObject" || action == "s3:ListBucket" || action == "s3:ListBucketVersions" || action == "s3:ListMultipartUploadParts"
	}
	return false
}

func grantsAllow(grants []meta.Grant, info *auth.AuthInfo, want string) bool {
	if want == "" {
		return false
	}
	for _, g := range grants {
		if !permissionCovers(g.Permission, want) {
			continue
		}
		if granteeMatches(g, info) {
			return true
		}
	}
	return false
}

func permissionCovers(have, want string) bool {
	return have == "FULL_CONTROL" || have == want
}

func granteeMatches(g meta.Grant, info *auth.AuthInfo) bool {
	switch g.GranteeType {
	case "Group":
		switch g.URI {
		case groupAllUsers:
			return true
		case groupAuthenticatedUsers:
			return info != nil && !info.IsAnonymous
		}
	case "CanonicalUser":
		if info == nil || info.Owner == "" {
			return false
		}
		return info.Owner == g.ID
	}
	return false
}
