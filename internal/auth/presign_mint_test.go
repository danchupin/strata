package auth

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestGeneratePresignedURLRoundTrip(t *testing.T) {
	now := time.Now().UTC()
	urlStr, err := GeneratePresignedURL(PresignOptions{
		Method:    http.MethodPut,
		Scheme:    "http",
		Host:      "strata.local:9000",
		Path:      "/bkt/my object.txt",
		Region:    "us-east-1",
		AccessKey: "AKIAEXAMPLE",
		Secret:    "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		Expires:   5 * time.Minute,
		Now:       now,
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if urlStr == "" {
		t.Fatalf("empty url")
	}
	u, err := url.Parse(urlStr)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := u.Query().Get("X-Amz-Algorithm"); got != sigAlgorithm {
		t.Fatalf("algorithm = %q want %q", got, sigAlgorithm)
	}
	if got := u.Query().Get("X-Amz-SignedHeaders"); got != "host" {
		t.Fatalf("signed_headers = %q want host", got)
	}
	if u.Query().Get("X-Amz-Signature") == "" {
		t.Fatalf("missing X-Amz-Signature")
	}

	// Build the inbound request the gateway would see and verify it via the
	// existing middleware path.
	req := httptest.NewRequest(http.MethodPut, urlStr, nil)
	req.Host = u.Host
	mw := &Middleware{Store: &stubStore{cred: &Credential{
		AccessKey: "AKIAEXAMPLE",
		Secret:    "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		Owner:     "owner",
	}}, Mode: ModeRequired}
	if !hasPresignedParams(req) {
		t.Fatalf("hasPresignedParams=false")
	}
	info, err := mw.validatePresigned(req)
	if err != nil {
		t.Fatalf("validatePresigned: %v", err)
	}
	if info.AccessKey != "AKIAEXAMPLE" || info.Owner != "owner" {
		t.Fatalf("info=%+v", info)
	}
}

func TestGeneratePresignedURLRequiredFields(t *testing.T) {
	if _, err := GeneratePresignedURL(PresignOptions{}); err == nil {
		t.Fatalf("expected error on empty opts")
	}
}

