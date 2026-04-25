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
	if info == nil || info.Anonymous {
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
