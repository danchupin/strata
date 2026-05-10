// genbaseline drives racetest against an in-process strata gateway and
// emits a SUMMARY.md the developer can paste into docs/racecheck/.
// Used to refresh the local baseline outside of GitHub Actions; the
// canonical baseline still comes from the nightly race-nightly workflow.
//
// Usage:
//
//	go run ./scripts/racecheck/genbaseline -duration=60s -dir=/tmp/baseline
//	REPORT_DIR=/tmp/baseline scripts/racecheck/summarize.sh
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"time"

	"github.com/danchupin/strata/internal/auth"
	datamem "github.com/danchupin/strata/internal/data/memory"
	metamem "github.com/danchupin/strata/internal/meta/memory"
	"github.com/danchupin/strata/internal/racetest"
	"github.com/danchupin/strata/internal/s3api"
)

func main() {
	duration := flag.Duration("duration", 60*time.Second, "harness wall-clock window")
	concurrency := flag.Int("concurrency", 16, "worker goroutines")
	buckets := flag.Int("buckets", 4, "bucket cardinality")
	keys := flag.Int("keys-per-bucket", 16, "per-bucket keys")
	dir := flag.String("dir", "report", "output dir for race.jsonl + host.txt")
	flag.Parse()

	if err := os.MkdirAll(*dir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "mkdir:", err)
		os.Exit(2)
	}

	const (
		ak = "AKIAGENBASELINE000000"
		sk = "secretsecretsecretsecretsecret00"
	)
	store := auth.NewStaticStore(map[string]*auth.Credential{
		ak: {AccessKey: ak, Secret: sk, Owner: "baseline"},
	})
	multi := auth.NewMultiStore(time.Minute, store)
	mw := &auth.Middleware{Store: multi, Mode: auth.ModeRequired}

	api := s3api.New(datamem.New(), metamem.New())
	api.Region = "us-east-1"
	ts := httptest.NewServer(mw.Wrap(api, s3api.NewAuthDenyHandler(api.Meta)))
	defer ts.Close()

	host := filepath.Join(*dir, "host.txt")
	writeHostStub(host, "pre-load")

	ctx, cancel := context.WithTimeout(context.Background(), *duration+30*time.Second)
	defer cancel()

	report, err := racetest.Run(ctx, racetest.Config{
		HTTPEndpoint: ts.URL,
		Duration:     *duration,
		Concurrency:  *concurrency,
		BucketCount:  *buckets,
		ObjectKeys:   *keys,
		AccessKey:    ak,
		SecretKey:    sk,
		Region:       "us-east-1",
		ReportPath:   filepath.Join(*dir, "race.jsonl"),
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "racetest.Run:", err)
		os.Exit(2)
	}

	writeHostStub(host, "post-load")

	fmt.Printf("ops=%d inconsistencies=%d duration=%s\n",
		sumOps(report.OpsByClass), len(report.Inconsistencies), report.Duration)
	if len(report.Inconsistencies) > 0 {
		os.Exit(1)
	}
}

func sumOps(m map[string]int64) int64 {
	var total int64
	for _, n := range m {
		total += n
	}
	return total
}

// writeHostStub mirrors the section shape run.sh writes, so summarize.sh
// finds pre-load / post-load disk + mem rows. Values are static — the
// in-process baseline is not measuring host pressure.
func writeHostStub(path, label string) {
	now := time.Now().UTC().Format(time.RFC3339)
	block := fmt.Sprintf(`== %s %s
-- df -h /
Filesystem      Size  Used Avail Use%% Mounted on
/dev/null       1.0G  100M  900M   10%% /
-- free -m
              total        used        free      shared  buff/cache   available
Mem:           7000        2000        4000           0        1000        4500

`, label, now)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(block)
}
