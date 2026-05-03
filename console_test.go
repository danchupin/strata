package strata

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestConsoleHandlerServesIndex(t *testing.T) {
	srv := httptest.NewServer(http.StripPrefix("", ConsoleHandler()))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/console/")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content-type = %q, want text/html…", ct)
	}
}

func TestConsoleHandlerSPAFallback(t *testing.T) {
	srv := httptest.NewServer(ConsoleHandler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/console/buckets/some-deep-route")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (SPA fallback)", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("fallback content-type = %q, want text/html…", ct)
	}
}

func TestConsoleHandlerServesAsset(t *testing.T) {
	srv := httptest.NewServer(ConsoleHandler())
	defer srv.Close()

	// Locate the hashed asset from the embedded FS.
	root := ConsoleFS()
	entries, err := assetsList(root)
	if err != nil {
		t.Fatalf("assets list: %v", err)
	}
	if len(entries) == 0 {
		t.Skip("no built assets to verify (run make web-build)")
	}
	resp, err := http.Get(srv.URL + "/console/assets/" + entries[0])
	if err != nil {
		t.Fatalf("get asset: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("asset status = %d, want 200", resp.StatusCode)
	}
}
