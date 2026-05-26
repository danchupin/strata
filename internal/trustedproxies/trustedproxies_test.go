package trustedproxies_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/danchupin/strata/internal/trustedproxies"
)

func TestParse(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		tp, err := trustedproxies.Parse("")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !tp.Empty() {
			t.Fatalf("expected empty")
		}
	})

	t.Run("ipv4 + ipv6", func(t *testing.T) {
		tp, err := trustedproxies.Parse("10.0.0.0/8, 192.168.0.0/16, fd00::/8")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if tp.Empty() {
			t.Fatalf("expected non-empty")
		}
		cases := []struct {
			addr    string
			trusted bool
		}{
			{"10.1.2.3:5555", true},
			{"192.168.10.10:80", true},
			{"[fd00::1]:443", true},
			{"127.0.0.1:80", false},
			{"8.8.8.8:80", false},
			{"[2001:db8::1]:80", false},
			{"", false},
			{"not-an-ip", false},
		}
		for _, c := range cases {
			if got := tp.Contains(c.addr); got != c.trusted {
				t.Errorf("Contains(%q): got=%v want=%v", c.addr, got, c.trusted)
			}
		}
	})

	t.Run("malformed CIDR rejected", func(t *testing.T) {
		_, err := trustedproxies.Parse("10.0.0.0/8,not-a-cidr,192.168.0.0/16")
		if err == nil {
			t.Fatalf("expected error")
		}
		if !strings.Contains(err.Error(), "not-a-cidr") {
			t.Fatalf("error should name the bad entry: %v", err)
		}
	})

	t.Run("bare IP rejected (must be CIDR)", func(t *testing.T) {
		_, err := trustedproxies.Parse("10.0.0.1")
		if err == nil {
			t.Fatalf("expected error for non-CIDR entry")
		}
	})
}

func TestContainsNilReceiver(t *testing.T) {
	var tp *trustedproxies.TrustedProxies
	if tp.Contains("10.0.0.1:80") {
		t.Fatalf("nil receiver must treat everything as untrusted")
	}
	if !tp.Empty() {
		t.Fatalf("nil receiver must be empty")
	}
}

func TestClientIP(t *testing.T) {
	t.Run("empty list ignores forwarded headers", func(t *testing.T) {
		tp, _ := trustedproxies.Parse("")
		r := newReq("10.0.0.1:5555", "X-Forwarded-For", "1.2.3.4")
		if got := tp.ClientIP(r); got != "10.0.0.1" {
			t.Fatalf("got=%q want=10.0.0.1", got)
		}
	})

	t.Run("trusted source honors XFF", func(t *testing.T) {
		tp, _ := trustedproxies.Parse("10.0.0.0/8")
		r := newReq("10.0.0.7:5555", "X-Forwarded-For", "203.0.113.7")
		if got := tp.ClientIP(r); got != "203.0.113.7" {
			t.Fatalf("got=%q want=203.0.113.7", got)
		}
	})

	t.Run("trusted source walks XFF left-to-right", func(t *testing.T) {
		tp, _ := trustedproxies.Parse("10.0.0.0/8")
		r := newReq("10.0.0.7:5555", "X-Forwarded-For", "203.0.113.7, 10.0.0.99")
		if got := tp.ClientIP(r); got != "203.0.113.7" {
			t.Fatalf("got=%q want=203.0.113.7", got)
		}
	})

	t.Run("trusted source skips trusted hops, returns first untrusted", func(t *testing.T) {
		tp, _ := trustedproxies.Parse("10.0.0.0/8")
		r := newReq("10.0.0.7:5555", "X-Forwarded-For", "10.0.0.99, 203.0.113.7, 10.0.0.5")
		if got := tp.ClientIP(r); got != "203.0.113.7" {
			t.Fatalf("got=%q want=203.0.113.7", got)
		}
	})

	t.Run("untrusted source ignores XFF", func(t *testing.T) {
		tp, _ := trustedproxies.Parse("10.0.0.0/8")
		r := newReq("8.8.8.8:5555", "X-Forwarded-For", "203.0.113.7")
		if got := tp.ClientIP(r); got != "8.8.8.8" {
			t.Fatalf("got=%q want=8.8.8.8", got)
		}
	})

	t.Run("trusted source falls back to X-Real-IP when XFF absent", func(t *testing.T) {
		tp, _ := trustedproxies.Parse("10.0.0.0/8")
		r := newReq("10.0.0.7:5555", "X-Real-IP", "203.0.113.7")
		if got := tp.ClientIP(r); got != "203.0.113.7" {
			t.Fatalf("got=%q want=203.0.113.7", got)
		}
	})
}

func TestForwardedProto(t *testing.T) {
	t.Run("empty list ignores header", func(t *testing.T) {
		tp, _ := trustedproxies.Parse("")
		r := newReq("10.0.0.7:5555", "X-Forwarded-Proto", "https")
		if got := tp.ForwardedProto(r); got != "" {
			t.Fatalf("got=%q want empty", got)
		}
	})

	t.Run("trusted source returns proto", func(t *testing.T) {
		tp, _ := trustedproxies.Parse("10.0.0.0/8")
		r := newReq("10.0.0.7:5555", "X-Forwarded-Proto", "https")
		if got := tp.ForwardedProto(r); got != "https" {
			t.Fatalf("got=%q want=https", got)
		}
	})

	t.Run("untrusted source rejects header", func(t *testing.T) {
		tp, _ := trustedproxies.Parse("10.0.0.0/8")
		r := newReq("8.8.8.8:5555", "X-Forwarded-Proto", "https")
		if got := tp.ForwardedProto(r); got != "" {
			t.Fatalf("got=%q want empty", got)
		}
	})

	t.Run("unknown proto value ignored", func(t *testing.T) {
		tp, _ := trustedproxies.Parse("10.0.0.0/8")
		r := newReq("10.0.0.7:5555", "X-Forwarded-Proto", "ftp")
		if got := tp.ForwardedProto(r); got != "" {
			t.Fatalf("got=%q want empty (unknown proto)", got)
		}
	})
}

func newReq(remoteAddr string, hdrs ...string) *http.Request {
	r, _ := http.NewRequest(http.MethodGet, "http://example.com/", nil)
	r.RemoteAddr = remoteAddr
	for i := 0; i+1 < len(hdrs); i += 2 {
		r.Header.Set(hdrs[i], hdrs[i+1])
	}
	return r
}
