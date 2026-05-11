package s3

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/danchupin/strata/internal/data"
)

// TestStubCORSReturnsErrUnsupported pins the contract: a zero-value
// Backend (no clusters) must surface errors.ErrUnsupported on every
// CORSBackend method before Open wires a live client (US-015).
func TestStubCORSReturnsErrUnsupported(t *testing.T) {
	b := &Backend{}
	ctx := context.Background()

	if err := b.PutBackendCORS(ctx, nil); !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("PutBackendCORS: want errors.ErrUnsupported, got %v", err)
	}
	if _, err := b.GetBackendCORS(ctx); !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("GetBackendCORS: want errors.ErrUnsupported, got %v", err)
	}
	if err := b.DeleteBackendCORS(ctx); !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("DeleteBackendCORS: want errors.ErrUnsupported, got %v", err)
	}
}

// TestPutBackendCORSSendsRules pins the AC: pushed rules surface in the
// SDK's CORSConfiguration XML body; ID, methods, origins, headers, expose,
// max-age all round-trip.
func TestPutBackendCORSSendsRules(t *testing.T) {
	ctx := context.Background()
	captured := newCORSRoundTripper()
	b, err := Open(ctx, openConfig(captured))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	rules := []data.CORSRule{
		{
			ID:             "r1",
			AllowedMethods: []string{"GET", "PUT"},
			AllowedOrigins: []string{"https://example.com"},
			AllowedHeaders: []string{"x-amz-*"},
			ExposeHeaders:  []string{"ETag"},
			MaxAgeSeconds:  3000,
		},
	}
	if err := b.PutBackendCORS(ctx, rules); err != nil {
		t.Fatalf("PutBackendCORS: %v", err)
	}
	body := captured.lastBody()
	for _, want := range []string{
		"<ID>r1</ID>",
		"<AllowedMethod>GET</AllowedMethod>",
		"<AllowedMethod>PUT</AllowedMethod>",
		"<AllowedOrigin>https://example.com</AllowedOrigin>",
		"<AllowedHeader>x-amz-*</AllowedHeader>",
		"<ExposeHeader>ETag</ExposeHeader>",
		"<MaxAgeSeconds>3000</MaxAgeSeconds>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("backend cors body missing %q: %s", want, body)
		}
	}
	if got := captured.lastQuery(); !strings.Contains(got, "cors") {
		t.Errorf("backend call must hit ?cors, got %q", got)
	}
	if got := captured.lastMethod(); got != http.MethodPut {
		t.Errorf("expected PUT, got %s", got)
	}
}

// TestPutBackendCORSEmptyRulesClearsBackend pins the AC: empty rules
// translate to a DELETE on the backend (S3 rejects empty CORSConfiguration).
func TestPutBackendCORSEmptyRulesClearsBackend(t *testing.T) {
	ctx := context.Background()
	captured := newCORSRoundTripper()
	b, err := Open(ctx, openConfig(captured))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if err := b.PutBackendCORS(ctx, nil); err != nil {
		t.Fatalf("PutBackendCORS(nil): %v", err)
	}
	if got := captured.lastMethod(); got != http.MethodDelete {
		t.Errorf("expected DELETE on empty rules, got %s", got)
	}
}

// TestGetBackendCORSParsesResponse drives Get against a synthetic backend
// response and asserts the SDK-shape rules are translated back to
// data.CORSRule form correctly.
func TestGetBackendCORSParsesResponse(t *testing.T) {
	ctx := context.Background()
	rt := newCORSRoundTripper()
	rt.getBody = `<?xml version="1.0" encoding="UTF-8"?>
<CORSConfiguration>
  <CORSRule>
    <ID>backend-rule</ID>
    <AllowedMethod>GET</AllowedMethod>
    <AllowedOrigin>https://x.com</AllowedOrigin>
    <AllowedHeader>x-foo</AllowedHeader>
    <ExposeHeader>ETag</ExposeHeader>
    <MaxAgeSeconds>600</MaxAgeSeconds>
  </CORSRule>
</CORSConfiguration>`
	b, err := Open(ctx, openConfig(rt))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	rules, err := b.GetBackendCORS(ctx)
	if err != nil {
		t.Fatalf("GetBackendCORS: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("rules: want 1, got %d", len(rules))
	}
	r := rules[0]
	if r.ID != "backend-rule" || r.MaxAgeSeconds != 600 {
		t.Errorf("rule mistranslated: %+v", r)
	}
	if len(r.AllowedMethods) != 1 || r.AllowedMethods[0] != "GET" {
		t.Errorf("methods mistranslated: %v", r.AllowedMethods)
	}
	if len(r.AllowedOrigins) != 1 || r.AllowedOrigins[0] != "https://x.com" {
		t.Errorf("origins mistranslated: %v", r.AllowedOrigins)
	}
}

// TestGetBackendCORSNoSuchCORSReturnsEmpty pins idempotent semantics: a
// backend that has never been configured surfaces NoSuchCORSConfiguration,
// which we treat as zero rules with no error.
func TestGetBackendCORSNoSuchCORSReturnsEmpty(t *testing.T) {
	ctx := context.Background()
	rt := newCORSRoundTripper()
	rt.getStatus = http.StatusNotFound
	rt.getBody = `<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>NoSuchCORSConfiguration</Code><Message>The CORS configuration does not exist</Message></Error>`
	b, err := Open(ctx, openConfig(rt))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	rules, err := b.GetBackendCORS(ctx)
	if err != nil {
		t.Fatalf("GetBackendCORS: want nil err on NoSuchCORS, got %v", err)
	}
	if len(rules) != 0 {
		t.Errorf("rules: want 0, got %d", len(rules))
	}
}

// TestDeleteBackendCORSIssuesDelete pins the symmetric clear path.
func TestDeleteBackendCORSIssuesDelete(t *testing.T) {
	ctx := context.Background()
	rt := newCORSRoundTripper()
	b, err := Open(ctx, openConfig(rt))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if err := b.DeleteBackendCORS(ctx); err != nil {
		t.Fatalf("DeleteBackendCORS: %v", err)
	}
	if got := rt.lastMethod(); got != http.MethodDelete {
		t.Errorf("expected DELETE, got %s", got)
	}
	if got := rt.lastQuery(); !strings.Contains(got, "cors") {
		t.Errorf("expected ?cors, got %q", got)
	}
}

// TestDeleteBackendCORSNoSuchCORSIdempotent: NoSuchCORSConfiguration on
// delete is success.
func TestDeleteBackendCORSNoSuchCORSIdempotent(t *testing.T) {
	ctx := context.Background()
	rt := newCORSRoundTripper()
	rt.deleteStatus = http.StatusNotFound
	rt.deleteBody = `<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>NoSuchCORSConfiguration</Code><Message>nope</Message></Error>`
	b, err := Open(ctx, openConfig(rt))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if err := b.DeleteBackendCORS(ctx); err != nil {
		t.Fatalf("DeleteBackendCORS: want nil on NoSuchCORS, got %v", err)
	}
}

// corsRoundTripper records the most recent request and replays a fixed
// response per method. PUT/DELETE default to 200 OK with empty body; GET
// defaults to 200 OK with getBody (caller fills) — separate test fields
// so a single transport handles all three CORS sub-ops in one Open().
type corsRoundTripper struct {
	method string
	query  string
	body   string

	getStatus    int
	getBody      string
	deleteStatus int
	deleteBody   string
}

func newCORSRoundTripper() *corsRoundTripper { return &corsRoundTripper{} }

func (t *corsRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		raw, _ := io.ReadAll(req.Body)
		t.body = string(raw)
		_ = req.Body.Close()
	}
	t.method = req.Method
	t.query = req.URL.RawQuery

	status := http.StatusOK
	body := ""
	switch req.Method {
	case http.MethodGet:
		body = t.getBody
		if t.getStatus != 0 {
			status = t.getStatus
		}
	case http.MethodDelete:
		if t.deleteStatus != 0 {
			status = t.deleteStatus
		}
		body = t.deleteBody
	}
	resp := &http.Response{
		Status:     http.StatusText(status),
		StatusCode: status,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     http.Header{"Content-Type": []string{"application/xml"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
	return resp, nil
}

func (t *corsRoundTripper) lastMethod() string { return t.method }
func (t *corsRoundTripper) lastQuery() string  { return t.query }
func (t *corsRoundTripper) lastBody() string   { return t.body }
