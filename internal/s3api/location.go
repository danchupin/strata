package s3api

import (
	"encoding/xml"
	"io"
	"net/http"

	"github.com/danchupin/strata/internal/meta"
)

// createBucketConfiguration mirrors the AWS CreateBucket request body.
type createBucketConfiguration struct {
	XMLName            xml.Name `xml:"CreateBucketConfiguration"`
	LocationConstraint string   `xml:"LocationConstraint"`
}

// locationConstraintResponse is the AWS GetBucketLocation response body.
// The XML namespace matches the canonical AWS S3 namespace; the inner text is
// empty for the default region (us-east-1 historically, "default" here).
type locationConstraintResponse struct {
	XMLName xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ LocationConstraint"`
	Value   string   `xml:",chardata"`
}

// parseCreateBucketLocation returns the LocationConstraint from the request
// body. Empty body or absent constraint returns "" (no region preference).
func parseCreateBucketLocation(r *http.Request) (string, APIError, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		return "", ErrMalformedXML, false
	}
	if len(body) == 0 {
		return "", APIError{}, true
	}
	var cfg createBucketConfiguration
	if err := xml.Unmarshal(body, &cfg); err != nil {
		return "", ErrMalformedXML, false
	}
	return cfg.LocationConstraint, APIError{}, true
}

func (s *Server) getBucketLocation(w http.ResponseWriter, r *http.Request, bucket string) {
	b, err := s.Meta.GetBucket(r.Context(), bucket)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	writeXML(w, http.StatusOK, locationConstraintResponse{Value: b.Region})
}

// bucketRegionFor returns the region label exposed via x-amz-bucket-region.
// Buckets without a stored region fall back to the gateway's configured region.
func (s *Server) bucketRegionFor(b *meta.Bucket) string {
	if b.Region != "" {
		return b.Region
	}
	if s.Region != "" {
		return s.Region
	}
	return "default"
}
