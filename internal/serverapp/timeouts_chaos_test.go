package serverapp

import (
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/danchupin/strata/internal/config"
)

// TestSlowlorisDropsConnection drives a Go-native slowloris probe (no
// shell `nc` — BusyBox semantics differ): opens a raw TCP socket and
// dribbles header bytes past ReadHeaderTimeout. Asserts the server has
// closed the connection by the time a final read fires its deadline,
// proving the per-connection timeout is wired through US-001.
//
// US-010 smoke harness re-runs this via `go test -run TestSlowloris
// ./internal/serverapp/` so the assertion stays portable across the
// alpine-glibc smoke image and the developer laptop.
func TestSlowlorisDropsConnection(t *testing.T) {
	if testing.Short() {
		t.Skip("slowloris chaos test skipped under -short")
	}
	cfg := &config.Config{HTTP: config.HTTPConfig{
		ReadHeaderTimeout: 200 * time.Millisecond,
		ReadTimeout:       200 * time.Millisecond,
		WriteTimeout:      5 * time.Second,
		IdleTimeout:       1 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	srv := newHTTPServer(ln.Addr().String(), handler, cfg)
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		_ = srv.Serve(ln)
	}()
	t.Cleanup(func() {
		_ = srv.Close()
		<-serveDone
	})

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	payload := []byte("GET / HTTP/1.1\r\nHost: slowloris.test\r\n")
	dribbleBudget := cfg.HTTP.ReadHeaderTimeout * 3
	end := time.Now().Add(dribbleBudget)
	idx := 0
	for time.Now().Before(end) && idx < len(payload) {
		time.Sleep(100 * time.Millisecond)
		if _, werr := conn.Write(payload[idx : idx+1]); werr != nil {
			break
		}
		idx++
	}

	// Once the dribble budget elapses, the server must have closed the
	// connection. io.ReadAll returns nil on EOF (Go convention), surfaces
	// a non-timeout error on RST/broken-pipe, and a deadline error only
	// when the server kept the socket open past the slack window.
	conn.SetReadDeadline(time.Now().Add(cfg.HTTP.ReadHeaderTimeout + 5*time.Second))
	_, rerr := io.ReadAll(conn)
	if rerr == nil {
		return
	}
	var nerr net.Error
	if errors.As(rerr, &nerr) && nerr.Timeout() {
		t.Fatalf("slowloris read deadline elapsed; server kept connection open past %s (header timeout + slack)",
			cfg.HTTP.ReadHeaderTimeout+5*time.Second)
	}
}
