package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	datamem "github.com/danchupin/strata/internal/data/memory"
	metamem "github.com/danchupin/strata/internal/meta/memory"
	"github.com/danchupin/strata/internal/s3api"
)

// readJSONL extracts the per-line `event` value from a JSON-lines file
// the binary wrote via --report. Used to assert which event kinds the
// runner emitted; we don't care about the rest of the payload here.
func readJSONL(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var kinds []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		var row struct {
			Event string `json:"event"`
		}
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			return nil, err
		}
		kinds = append(kinds, row.Event)
	}
	return kinds, sc.Err()
}

// TestRunSmokeBinary drives the strata-racecheck main loop against an
// in-process gateway, asserting (a) clean exit code 0, (b) the JSON-lines
// report file is populated, and (c) the human summary printed to stdout
// names the expected op classes. This is the regression gate that the
// flag wiring + exit-code mapping does not silently rot.
func TestRunSmokeBinary(t *testing.T) {
	api := s3api.New(datamem.New(), metamem.New())
	api.Region = "default"
	ts := httptest.NewServer(http.HandlerFunc(api.ServeHTTP))
	t.Cleanup(ts.Close)

	report := filepath.Join(t.TempDir(), "race.jsonl")
	var stdout, stderr bytes.Buffer

	args := []string{
		"--endpoint=" + ts.URL,
		"--duration=1500ms",
		"--concurrency=4",
		"--buckets=2",
		"--keys-per-bucket=4",
		"--report=" + report,
	}
	rc := run(args, &stdout, &stderr)
	if rc != exitOK {
		t.Fatalf("exit code: got %d want %d; stderr=%s", rc, exitOK, stderr.String())
	}

	// Stdout should name the full workload mix per the US-003 op
	// classes — both the original PUT/DELETE/multipart trio and the
	// new GET/list/versioning_flip/conditional_put/delete_objects ops.
	for _, want := range []string{
		"strata-racecheck summary", "ops total:",
		"put", "get", "delete", "list",
		"multipart", "versioning_flip", "conditional_put", "delete_objects",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout missing %q; got %s", want, stdout.String())
		}
	}

	got, err := readJSONL(report)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("expected events in report file, got none")
	}
	saw := map[string]int{}
	for _, ev := range got {
		saw[ev]++
	}
	for _, kind := range []string{"op_started", "op_done", "summary"} {
		if saw[kind] == 0 {
			t.Errorf("event kind %q missing from report (saw=%v)", kind, saw)
		}
	}
}

// TestBadConfigExitsTwo asserts setup-level failures (zero duration,
// blank endpoint) exit 2. Stdout is allowed to be empty; stderr carries
// the diagnostic.
func TestBadConfigExitsTwo(t *testing.T) {
	cases := [][]string{
		{"--duration=0", "--concurrency=1"},
		{"--endpoint=http://x", "--duration=0", "--concurrency=1"},
	}
	for _, args := range cases {
		var stdout, stderr bytes.Buffer
		if rc := run(args, &stdout, &stderr); rc != exitSetupFailure {
			t.Errorf("args=%v exit=%d want %d", args, rc, exitSetupFailure)
		}
	}
}
