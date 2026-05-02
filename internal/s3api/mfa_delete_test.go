package s3api_test

import (
	"encoding/base32"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	datamem "github.com/danchupin/strata/internal/data/memory"
	metamem "github.com/danchupin/strata/internal/meta/memory"
	"github.com/danchupin/strata/internal/s3api"
)

const (
	mfaTestSerial = "arn:aws:iam::123:mfa/root"
	mfaTestSecret = "JBSWY3DPEHPK3PXP" // base32 "Hello!\xde\xad\xbe\xef"
)

func newMFAHarness(t *testing.T) (*httptest.Server, *s3api.Server) {
	t.Helper()
	api := s3api.New(datamem.New(), metamem.New())
	api.Region = "default"
	secrets, err := s3api.ParseMFASecrets(mfaTestSerial + ":" + mfaTestSecret)
	if err != nil {
		t.Fatalf("parse secrets: %v", err)
	}
	api.MFASecrets = secrets
	fixed := time.Unix(1700000000, 0).UTC()
	api.MFAClock = func() time.Time { return fixed }
	ts := httptest.NewServer(api)
	t.Cleanup(ts.Close)
	return ts, api
}

func mfaDo(t *testing.T, ts *httptest.Server, method, path, body string, headers ...string) *http.Response {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, ts.URL+path, r)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	return resp
}

func mfaPutObject(t *testing.T, ts *httptest.Server, bucket, key string) string {
	t.Helper()
	resp := mfaDo(t, ts, http.MethodPut, "/"+bucket+"/"+key, "hello")
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("put: %d %s", resp.StatusCode, body)
	}
	versionID := resp.Header.Get("x-amz-version-id")
	_ = resp.Body.Close()
	return versionID
}

func setupMFABucket(t *testing.T, ts *httptest.Server, bucket string, mfaState string) {
	t.Helper()
	resp := mfaDo(t, ts, http.MethodPut, "/"+bucket, "")
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("create bucket: %d %s", resp.StatusCode, body)
	}
	_ = resp.Body.Close()

	versioningBody := `<VersioningConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Status>Enabled</Status>`
	if mfaState != "" {
		versioningBody += "<MfaDelete>" + mfaState + "</MfaDelete>"
	}
	versioningBody += "</VersioningConfiguration>"
	resp = mfaDo(t, ts, http.MethodPut, "/"+bucket+"?versioning", versioningBody)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("put versioning: %d %s", resp.StatusCode, body)
	}
	_ = resp.Body.Close()
}

func decodeMFASecret(t *testing.T) []byte {
	t.Helper()
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(mfaTestSecret)
	if err != nil {
		t.Fatalf("decode secret: %v", err)
	}
	return key
}

func mfaCode(t *testing.T, at time.Time) string {
	t.Helper()
	return s3api.TOTPForTest(decodeMFASecret(t), at)
}

func TestMFADeleteEnabledMissingHeaderRejected(t *testing.T) {
	ts, _ := newMFAHarness(t)
	setupMFABucket(t, ts, "mfb", "Enabled")
	versionID := mfaPutObject(t, ts, "mfb", "obj")

	resp := mfaDo(t, ts, http.MethodDelete, "/mfb/obj?versionId="+versionID, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("missing header: got %d want 403; body=%s", resp.StatusCode, body)
	}
}

func TestMFADeleteEnabledValidTOTPAccepted(t *testing.T) {
	ts, api := newMFAHarness(t)
	setupMFABucket(t, ts, "mfb", "Enabled")
	versionID := mfaPutObject(t, ts, "mfb", "obj")

	code := mfaCode(t, api.MFAClock())
	resp := mfaDo(t, ts, http.MethodDelete, "/mfb/obj?versionId="+versionID, "",
		"x-amz-mfa", mfaTestSerial+" "+code,
	)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("valid totp: got %d want 204; body=%s", resp.StatusCode, body)
	}
}

func TestMFADeleteEnabledInvalidTOTPRejected(t *testing.T) {
	ts, _ := newMFAHarness(t)
	setupMFABucket(t, ts, "mfb", "Enabled")
	versionID := mfaPutObject(t, ts, "mfb", "obj")

	resp := mfaDo(t, ts, http.MethodDelete, "/mfb/obj?versionId="+versionID, "",
		"x-amz-mfa", mfaTestSerial+" 000000",
	)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("bad totp: got %d want 403; body=%s", resp.StatusCode, body)
	}
}

func TestMFADeleteEnabledUnknownSerialRejected(t *testing.T) {
	ts, api := newMFAHarness(t)
	setupMFABucket(t, ts, "mfb", "Enabled")
	versionID := mfaPutObject(t, ts, "mfb", "obj")

	code := mfaCode(t, api.MFAClock())
	resp := mfaDo(t, ts, http.MethodDelete, "/mfb/obj?versionId="+versionID, "",
		"x-amz-mfa", "arn:aws:iam::999:mfa/other "+code,
	)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("unknown serial: got %d want 403; body=%s", resp.StatusCode, body)
	}
}

func TestMFADeleteDisabledNoHeaderRequired(t *testing.T) {
	ts, _ := newMFAHarness(t)
	setupMFABucket(t, ts, "mfb", "Disabled")
	versionID := mfaPutObject(t, ts, "mfb", "obj")

	resp := mfaDo(t, ts, http.MethodDelete, "/mfb/obj?versionId="+versionID, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("disabled state: got %d want 204; body=%s", resp.StatusCode, body)
	}
}

func TestMFADeleteUnconfiguredBucket(t *testing.T) {
	ts, _ := newMFAHarness(t)
	setupMFABucket(t, ts, "mfb", "")
	versionID := mfaPutObject(t, ts, "mfb", "obj")

	resp := mfaDo(t, ts, http.MethodDelete, "/mfb/obj?versionId="+versionID, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("unconfigured: got %d want 204; body=%s", resp.StatusCode, body)
	}
}

func TestMFADeleteVersioningRoundTrip(t *testing.T) {
	ts, _ := newMFAHarness(t)
	setupMFABucket(t, ts, "mfb", "Enabled")

	resp := mfaDo(t, ts, http.MethodGet, "/mfb?versioning", "")
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	got := string(body)
	if !strings.Contains(got, "<MfaDelete>Enabled</MfaDelete>") {
		t.Fatalf("MfaDelete not echoed: %s", got)
	}
	if !strings.Contains(got, "<Status>Enabled</Status>") {
		t.Fatalf("Status not echoed: %s", got)
	}
}

func TestParseMFASecretsErrors(t *testing.T) {
	if _, err := s3api.ParseMFASecrets("bad-no-colon"); err == nil {
		t.Fatalf("expected error for missing colon")
	}
	if _, err := s3api.ParseMFASecrets("serial:!!!notbase32"); err == nil {
		t.Fatalf("expected error for invalid base32")
	}
	out, err := s3api.ParseMFASecrets(""); if err != nil || len(out) != 0 {
		t.Fatalf("empty input should yield empty map, got %v err=%v", out, err)
	}
	out, err = s3api.ParseMFASecrets(mfaTestSerial+":"+mfaTestSecret)
	if err != nil || len(out) != 1 {
		t.Fatalf("single entry: got %v err=%v", out, err)
	}
}
