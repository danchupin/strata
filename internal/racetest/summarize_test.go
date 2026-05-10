package racetest

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestSummarizeScript exercises scripts/racecheck/summarize.sh against a
// synthetic race.jsonl + host.txt. The script is shell + jq; this test is
// the closest thing to a unit test that lives inside the Go suite.
func TestSummarizeScript(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not installed — skipping summarize.sh test")
	}
	script := summarizeScriptPath(t)

	t.Run("populated", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "race.jsonl"), populatedJSONL)
		writeFile(t, filepath.Join(dir, "host.txt"), populatedHost)
		runSummarize(t, script, dir)
		got := readFile(t, filepath.Join(dir, "SUMMARY.md"))
		assertContains(t, got, "# Race soak summary")
		assertContains(t, got, "**Status: inconsistencies detected (1).**")
		assertContains(t, got, "Total ops: `5`")
		assertContains(t, got, "Throughput: `125.0 ops/sec`")
		assertContains(t, got, "| put | 3 |")
		assertContains(t, got, "| get | 2 |")
		assertContains(t, got, "missing_version")
		assertContains(t, got, "rc-bkt-0/k-1")
		assertContains(t, got, "Disk used (`/`) | `42G` | `55G` |")
		assertContains(t, got, "Mem used (MiB) | `1500` | `2200` |")
	})

	t.Run("empty file", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "race.jsonl"), "")
		runSummarize(t, script, dir)
		got := readFile(t, filepath.Join(dir, "SUMMARY.md"))
		assertContains(t, got, "# Race soak summary")
		assertContains(t, got, "missing or empty")
	})

	t.Run("partial run no summary event", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "race.jsonl"), partialJSONL)
		runSummarize(t, script, dir)
		got := readFile(t, filepath.Join(dir, "SUMMARY.md"))
		assertContains(t, got, "Status: clean.")
		assertContains(t, got, "Duration: `n/a`")
		assertContains(t, got, "Throughput: `n/a`")
		assertContains(t, got, "| put | 2 |")
	})

	t.Run("more than three inconsistencies", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "race.jsonl"), manyInconsistJSONL)
		runSummarize(t, script, dir)
		got := readFile(t, filepath.Join(dir, "SUMMARY.md"))
		assertContains(t, got, "Inconsistencies: `5`")
		assertContains(t, got, "First 3 of 5")
		// Acceptance criterion: only first 3 examples emitted.
		count := strings.Count(got, "- **etag_mismatch**")
		if count != 3 {
			t.Fatalf("expected exactly 3 etag_mismatch bullet rows, got %d. SUMMARY.md:\n%s", count, got)
		}
	})
}

func summarizeScriptPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Join(filepath.Dir(thisFile), "..", "..")
	script := filepath.Join(root, "scripts", "racecheck", "summarize.sh")
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("summarize.sh not found at %s: %v", script, err)
	}
	return script
}

func runSummarize(t *testing.T, script, reportDir string) {
	t.Helper()
	cmd := exec.Command("bash", script)
	cmd.Env = append(os.Environ(), "REPORT_DIR="+reportDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("summarize.sh failed: %v\noutput:\n%s", err, out)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func assertContains(t *testing.T, got, want string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Fatalf("expected SUMMARY.md to contain %q\n--- got ---\n%s", want, got)
	}
}

const populatedJSONL = `{"event":"op_started","ts":"2026-05-10T19:30:00Z","worker_id":0,"class":"put"}
{"event":"op_done","ts":"2026-05-10T19:30:00.005Z","worker_id":0,"class":"put","status":200,"duration_ms":5}
{"event":"op_done","ts":"2026-05-10T19:30:00.012Z","worker_id":1,"class":"put","status":200,"duration_ms":8}
{"event":"op_done","ts":"2026-05-10T19:30:00.020Z","worker_id":2,"class":"put","status":200,"duration_ms":15}
{"event":"op_done","ts":"2026-05-10T19:30:00.025Z","worker_id":0,"class":"get","status":200,"duration_ms":3}
{"event":"op_done","ts":"2026-05-10T19:30:00.026Z","worker_id":1,"class":"get","status":200,"duration_ms":4}
{"event":"inconsistency","ts":"2026-05-10T19:30:00.030Z","inconsistency":{"kind":"missing_version","bucket":"rc-bkt-0","key":"k-1","detail":"version_id v123 not found","timestamp":"2026-05-10T19:30:00.030Z"}}
{"event":"summary","ts":"2026-05-10T19:30:00.040Z","summary":{"started_at":"2026-05-10T19:30:00Z","ended_at":"2026-05-10T19:30:00.040Z","duration_ns":40000000,"ops_by_class":{"put":3,"get":2},"inconsistencies_count":1}}
`

const partialJSONL = `{"event":"op_done","ts":"2026-05-10T19:30:00.005Z","worker_id":0,"class":"put","status":200,"duration_ms":5}
{"event":"op_done","ts":"2026-05-10T19:30:00.012Z","worker_id":1,"class":"put","status":200,"duration_ms":8}
`

const manyInconsistJSONL = `{"event":"op_done","ts":"2026-05-10T19:30:00Z","class":"put","duration_ms":1}
{"event":"inconsistency","ts":"2026-05-10T19:30:01Z","inconsistency":{"kind":"etag_mismatch","bucket":"rc-bkt-0","key":"k-1","detail":"etag drift A"}}
{"event":"inconsistency","ts":"2026-05-10T19:30:02Z","inconsistency":{"kind":"etag_mismatch","bucket":"rc-bkt-0","key":"k-2","detail":"etag drift B"}}
{"event":"inconsistency","ts":"2026-05-10T19:30:03Z","inconsistency":{"kind":"etag_mismatch","bucket":"rc-bkt-0","key":"k-3","detail":"etag drift C"}}
{"event":"inconsistency","ts":"2026-05-10T19:30:04Z","inconsistency":{"kind":"etag_mismatch","bucket":"rc-bkt-0","key":"k-4","detail":"etag drift D"}}
{"event":"inconsistency","ts":"2026-05-10T19:30:05Z","inconsistency":{"kind":"etag_mismatch","bucket":"rc-bkt-0","key":"k-5","detail":"etag drift E"}}
{"event":"summary","ts":"2026-05-10T19:30:06Z","summary":{"duration_ns":6000000000,"inconsistencies_count":5}}
`

const populatedHost = `== pre-readyz 2026-05-10T19:29:50Z
-- df -h /
Filesystem      Size  Used Avail Use% Mounted on
overlay         200G   40G  160G  20% /
-- free -m
              total        used        free      shared  buff/cache   available
Mem:           7976         200        7000         100         700        7500
Swap:          1024          0         1024

== pre-load 2026-05-10T19:30:00Z
-- df -h /
Filesystem      Size  Used Avail Use% Mounted on
overlay         200G   42G  158G  21% /
-- free -m
              total        used        free      shared  buff/cache   available
Mem:           7976        1500        4000         100        2000        5000
Swap:          1024          0         1024

== post-load 2026-05-10T20:30:00Z
-- df -h /
Filesystem      Size  Used Avail Use% Mounted on
overlay         200G   55G  145G  28% /
-- free -m
              total        used        free      shared  buff/cache   available
Mem:           7976        2200        3000         100        2700        4200
Swap:          1024          0         1024

`
