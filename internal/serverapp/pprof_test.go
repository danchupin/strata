package serverapp

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/danchupin/strata/cmd/strata/workers"
	"github.com/danchupin/strata/internal/config"
)

// TestPprofDisabledReturns404 verifies that with STRATA_PPROF_ENABLED unset
// (default), /debug/pprof/heap returns 404 on every listener.
func TestPprofDisabledReturns404(t *testing.T) {
	mainAddr := freePort(t)
	adminAddr := freePort(t)
	t.Setenv("STRATA_LISTEN", mainAddr)
	t.Setenv("STRATA_ADMIN_LISTEN", adminAddr)
	t.Setenv("STRATA_DATA_BACKEND", "memory")
	t.Setenv("STRATA_META_BACKEND", "memory")
	t.Setenv("STRATA_AUTH_MODE", "off")
	t.Setenv("STRATA_SHUTDOWN_WAIT", "2s")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	runErr := make(chan error, 1)
	go func() { runErr <- Run(runCtx, cfg, logger, []workers.Worker{}) }()
	waitListen(t, mainAddr)
	waitListen(t, adminAddr)

	client := &http.Client{Timeout: 3 * time.Second}
	for _, addr := range []string{mainAddr, adminAddr} {
		resp, err := client.Get("http://" + addr + "/debug/pprof/heap")
		if err != nil {
			t.Fatalf("get %s /debug/pprof/heap: %v", addr, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("disabled pprof on %s status=%d want 404", addr, resp.StatusCode)
		}
	}

	cancel()
	<-runErr
}

// TestPprofAdminListenerServes verifies that pprof handlers attach to the
// admin listener when STRATA_PPROF_ENABLED=true and STRATA_PPROF_LISTEN is
// empty. SigV4-authenticated requests succeed; anonymous requests are
// rejected with 401.
func TestPprofAdminListenerServes(t *testing.T) {
	mainAddr := freePort(t)
	adminAddr := freePort(t)
	t.Setenv("STRATA_LISTEN", mainAddr)
	t.Setenv("STRATA_ADMIN_LISTEN", adminAddr)
	t.Setenv("STRATA_DATA_BACKEND", "memory")
	t.Setenv("STRATA_META_BACKEND", "memory")
	t.Setenv("STRATA_AUTH_MODE", "required")
	t.Setenv("STRATA_STATIC_CREDENTIALS", "AKADMIN:SKADMIN:admin")
	t.Setenv("STRATA_PPROF_ENABLED", "true")
	t.Setenv("STRATA_PPROF_LISTEN", "")
	t.Setenv("STRATA_SHUTDOWN_WAIT", "2s")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	runErr := make(chan error, 1)
	go func() { runErr <- Run(runCtx, cfg, logger, []workers.Worker{}) }()
	waitListen(t, mainAddr)
	waitListen(t, adminAddr)

	client := &http.Client{Timeout: 5 * time.Second}

	// Anonymous → 401
	resp, err := client.Get("http://" + adminAddr + "/debug/pprof/heap")
	if err != nil {
		t.Fatalf("anon get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("anon /debug/pprof/heap status=%d want 401", resp.StatusCode)
	}

	// Authenticated → 200 + valid pprof bytes
	req, _ := http.NewRequest("GET", "http://"+adminAddr+"/debug/pprof/heap", nil)
	signSigV4(t, req, "AKADMIN", "SKADMIN", "strata-local")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("authed get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("authed /debug/pprof/heap status=%d want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if _, err := parsePprofBytes(body); err != nil {
		t.Errorf("pprof body decode: %v", err)
	}

	// pprof handlers MUST NOT leak onto the main (S3) listener.
	resp, err = client.Get("http://" + mainAddr + "/debug/pprof/heap")
	if err != nil {
		t.Fatalf("main get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Errorf("pprof leaked to main S3 listener (status=200)")
	}

	cancel()
	<-runErr
}

// TestPprofDedicatedListenerServes verifies STRATA_PPROF_LISTEN spawns a
// third listener that serves pprof; admin listener returns 404 for
// /debug/pprof/.
func TestPprofDedicatedListenerServes(t *testing.T) {
	mainAddr := freePort(t)
	adminAddr := freePort(t)
	pprofAddr := freePort(t)
	t.Setenv("STRATA_LISTEN", mainAddr)
	t.Setenv("STRATA_ADMIN_LISTEN", adminAddr)
	t.Setenv("STRATA_DATA_BACKEND", "memory")
	t.Setenv("STRATA_META_BACKEND", "memory")
	t.Setenv("STRATA_AUTH_MODE", "required")
	t.Setenv("STRATA_STATIC_CREDENTIALS", "AKADMIN:SKADMIN:admin")
	t.Setenv("STRATA_PPROF_ENABLED", "true")
	t.Setenv("STRATA_PPROF_LISTEN", pprofAddr)
	t.Setenv("STRATA_SHUTDOWN_WAIT", "2s")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	runErr := make(chan error, 1)
	go func() { runErr <- Run(runCtx, cfg, logger, []workers.Worker{}) }()
	waitListen(t, mainAddr)
	waitListen(t, adminAddr)
	waitListen(t, pprofAddr)

	client := &http.Client{Timeout: 5 * time.Second}

	// Dedicated listener serves pprof under admin auth.
	req, _ := http.NewRequest("GET", "http://"+pprofAddr+"/debug/pprof/heap", nil)
	signSigV4(t, req, "AKADMIN", "SKADMIN", "strata-local")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("dedicated authed get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("dedicated authed /debug/pprof/heap status=%d want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if _, err := parsePprofBytes(body); err != nil {
		t.Errorf("pprof body decode: %v", err)
	}

	// Admin listener MUST NOT also expose /debug/pprof/ when a dedicated
	// listener is set — exclusive routing.
	resp, err = client.Get("http://" + adminAddr + "/debug/pprof/heap")
	if err != nil {
		t.Fatalf("admin get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Errorf("pprof leaked to admin listener when STRATA_PPROF_LISTEN is set (status=200)")
	}

	// Dedicated listener also requires auth.
	resp, err = client.Get("http://" + pprofAddr + "/debug/pprof/heap")
	if err != nil {
		t.Fatalf("dedicated anon get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("dedicated anon /debug/pprof/heap status=%d want 401", resp.StatusCode)
	}

	cancel()
	<-runErr
}

// TestPprofEnabledRequiresListener verifies STRATA_PPROF_ENABLED=true with
// neither STRATA_PPROF_LISTEN nor STRATA_ADMIN_LISTEN set fails fast at
// config load time — pprof MUST NOT silently attach to the S3 hot path.
func TestPprofEnabledRequiresListener(t *testing.T) {
	t.Setenv("STRATA_PPROF_ENABLED", "true")
	t.Setenv("STRATA_PPROF_LISTEN", "")
	t.Setenv("STRATA_ADMIN_LISTEN", "")
	_, err := config.Load()
	if err == nil {
		t.Fatal("expected error when pprof enabled with no listener, got nil")
	}
	if !strings.Contains(err.Error(), "pprof") {
		t.Errorf("error message %q does not mention pprof", err.Error())
	}
}

// signSigV4 stamps an AWS SigV4 Authorization header onto req. Lifted from
// internal/s3api test helpers so this package stays self-contained.
func signSigV4(t *testing.T, req *http.Request, accessKey, secret, region string) {
	t.Helper()
	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	day := now.Format("20060102")
	req.Header.Set("X-Amz-Date", amzDate)
	if req.Header.Get("Host") == "" {
		req.Host = req.URL.Host
	}
	bodyHash := sha256Hex(nil)
	req.Header.Set("X-Amz-Content-Sha256", bodyHash)

	signedHeaders := []string{"host", "x-amz-content-sha256", "x-amz-date"}
	canonical := strings.Join([]string{
		req.Method,
		canonicalPath(req.URL.EscapedPath()),
		canonicalQuery(req.URL.RawQuery),
		canonicalHeaders(req, signedHeaders),
		strings.Join(signedHeaders, ";"),
		bodyHash,
	}, "\n")
	scope := day + "/" + region + "/s3/aws4_request"
	sts := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		sha256Hex([]byte(canonical)),
	}, "\n")
	kDate := hmacSHA256([]byte("AWS4"+secret), []byte(day))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte("s3"))
	kSigning := hmacSHA256(kService, []byte("aws4_request"))
	sig := hex.EncodeToString(hmacSHA256(kSigning, []byte(sts)))
	req.Header.Set("Authorization",
		"AWS4-HMAC-SHA256 Credential="+accessKey+"/"+scope+
			", SignedHeaders="+strings.Join(signedHeaders, ";")+
			", Signature="+sig,
	)
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func canonicalPath(p string) string {
	if p == "" {
		return "/"
	}
	return p
}

func canonicalQuery(raw string) string {
	if raw == "" {
		return ""
	}
	pairs := strings.Split(raw, "&")
	sort.Strings(pairs)
	return strings.Join(pairs, "&")
}

func canonicalHeaders(req *http.Request, signed []string) string {
	var b strings.Builder
	for _, h := range signed {
		v := req.Header.Get(h)
		if h == "host" {
			v = req.Host
			if v == "" {
				v = req.URL.Host
			}
		}
		b.WriteString(strings.ToLower(h))
		b.WriteByte(':')
		b.WriteString(strings.TrimSpace(v))
		b.WriteByte('\n')
	}
	return b.String()
}

// silenceUnusedErr keeps the linter quiet on the test imports without
// emitting nil-check noise in the happy paths.
var _ = errors.New
