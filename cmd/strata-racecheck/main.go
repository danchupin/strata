// strata-racecheck drives the internal/racetest workload against an
// already-running strata gateway over HTTP. It is the duration-bounded
// race harness called by `make race-soak` and the nightly CI job; the
// in-process Go test variant lives in internal/s3api/race_test.go and
// shares the same workload library.
//
// strata-racecheck is a developer/CI tool. It does NOT count as a third
// production binary against the consolidation goal — see
// docs/site/content/architecture/migrations/binary-consolidation.md
// "Non-Goals" for the rationale.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/danchupin/strata/internal/racetest"
)

// Exit codes:
//
//	0 — clean run, no inconsistencies
//	1 — run completed but the verifier reported one or more inconsistencies
//	2 — transport / setup error (could not bring up workload, signing failure,
//	    bucket creation failure, etc.). The race-soak script propagates this
//	    exit code so the nightly workflow flips red on infra failures too.
const (
	exitOK             = 0
	exitInconsistency  = 1
	exitSetupFailure   = 2
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("strata-racecheck", flag.ContinueOnError)
	fs.SetOutput(stderr)

	endpoint := fs.String("endpoint", "http://localhost:9999", "gateway base URL")
	duration := fs.Duration("duration", time.Hour, "wall-clock window each worker runs for")
	concurrency := fs.Int("concurrency", 32, "worker goroutines (refused if > 64)")
	buckets := fs.Int("buckets", 4, "number of buckets the workload spreads ops across")
	keysPer := fs.Int("keys-per-bucket", 8, "per-bucket key cardinality")
	report := fs.String("report", "", "JSON-lines events report path (empty = no events file)")
	accessKey := fs.String("access-key", os.Getenv("AWS_ACCESS_KEY_ID"), "SigV4 access key (defaults to $AWS_ACCESS_KEY_ID)")
	secretKey := fs.String("secret-key", os.Getenv("AWS_SECRET_ACCESS_KEY"), "SigV4 secret key (defaults to $AWS_SECRET_ACCESS_KEY)")
	region := fs.String("region", envOr("AWS_DEFAULT_REGION", "us-east-1"), "SigV4 region")

	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: strata-racecheck [flags]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitSetupFailure
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	cfg := racetest.Config{
		HTTPEndpoint: *endpoint,
		Duration:     *duration,
		Concurrency:  *concurrency,
		BucketCount:  *buckets,
		ObjectKeys:   *keysPer,
		AccessKey:    *accessKey,
		SecretKey:    *secretKey,
		Region:       *region,
		ReportPath:   *report,
	}

	rep, err := racetest.Run(ctx, cfg)
	if err != nil {
		fmt.Fprintln(stderr, "strata-racecheck:", err)
		return exitSetupFailure
	}

	printSummary(stdout, rep)
	if len(rep.Inconsistencies) > 0 {
		return exitInconsistency
	}
	return exitOK
}

func printSummary(w io.Writer, rep *racetest.Report) {
	fmt.Fprintln(w, "== strata-racecheck summary ==")
	fmt.Fprintf(w, "started:        %s\n", rep.StartedAt.Format(time.RFC3339))
	fmt.Fprintf(w, "ended:          %s\n", rep.EndedAt.Format(time.RFC3339))
	fmt.Fprintf(w, "wall duration:  %s\n", rep.Duration)
	var total int64
	for _, n := range rep.OpsByClass {
		total += n
	}
	fmt.Fprintf(w, "ops total:      %d\n", total)
	for _, class := range []string{"put", "delete", "multipart"} {
		fmt.Fprintf(w, "  %-10s    %d\n", class, rep.OpsByClass[class])
	}
	fmt.Fprintf(w, "inconsistencies: %d\n", len(rep.Inconsistencies))
	for i, inc := range rep.Inconsistencies {
		if i >= 5 {
			fmt.Fprintf(w, "  ... %d more\n", len(rep.Inconsistencies)-5)
			break
		}
		fmt.Fprintf(w, "  - kind=%s bucket=%s key=%s detail=%s\n",
			inc.Kind, inc.Bucket, inc.Key, inc.Detail)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
