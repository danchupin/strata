// Command pprof-fetch is a SigV4-signed curl-equivalent used by
// scripts/smoke-pprof.sh to capture a profile from an authenticated
// /debug/pprof/* endpoint without depending on aws-cli on the smoke
// host. Mirrors the signing logic in
// internal/serverapp/pprof_test.go::signSigV4.
package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

func main() {
	var (
		target    = flag.String("url", "", "endpoint to fetch (required)")
		accessKey = flag.String("access-key", "", "SigV4 access key (required)")
		secret    = flag.String("secret", "", "SigV4 secret key (required)")
		region    = flag.String("region", "strata-local", "SigV4 region")
		out       = flag.String("out", "", "output path (required)")
		timeout   = flag.Duration("timeout", 60*time.Second, "HTTP timeout")
	)
	flag.Parse()
	if *target == "" || *accessKey == "" || *secret == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "missing required flag (--url/--access-key/--secret/--out)")
		os.Exit(2)
	}

	u, err := url.Parse(*target)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse url: %v\n", err)
		os.Exit(2)
	}
	req, err := http.NewRequest("GET", u.String(), nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "build request: %v\n", err)
		os.Exit(2)
	}
	signSigV4(req, *accessKey, *secret, *region)

	client := &http.Client{Timeout: *timeout}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "request: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "status=%d body=%s\n", resp.StatusCode, string(body))
		os.Exit(1)
	}

	f, err := os.Create(*out)
	if err != nil {
		fmt.Fprintf(os.Stderr, "create %s: %v\n", *out, err)
		os.Exit(1)
	}
	defer f.Close()
	if _, err := io.Copy(f, resp.Body); err != nil {
		fmt.Fprintf(os.Stderr, "copy body: %v\n", err)
		os.Exit(1)
	}
}

func signSigV4(req *http.Request, accessKey, secret, region string) {
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
