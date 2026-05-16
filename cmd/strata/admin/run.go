// Package admin is the operator CLI dispatched as `strata admin`. Subcommands
// map onto IAM admin endpoints + the /admin/* HTTP surface (US-034). Output is
// human-readable by default; --json prints the raw response payload for
// scripting.
package admin

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

// Run is the entrypoint dispatched by `strata admin`. It parses `args`
// (subcommand + flags, not the bare `os.Args`), executes the requested
// operation against the gateway, and returns nil on success. On usage
// errors it returns ErrUsage after printing the help banner to stderr; on
// any other error it prints "strata admin: <err>" to stderr and returns
// the underlying error so the top-level dispatcher can map it to exit 2.
func Run(args []string) error {
	return RunWith(os.Stdout, os.Stderr, args)
}

// RunWith is the writer-injected variant of Run; the top-level dispatcher
// passes its own writers so tests can capture admin output without piping
// real stdout/stderr.
func RunWith(stdout, stderr io.Writer, args []string) error {
	a := newApp(stdout, stderr, args)
	if err := a.run(context.Background()); err != nil {
		if !errors.Is(err, ErrUsage) {
			fmt.Fprintln(stderr, "strata admin:", err)
		}
		return err
	}
	return nil
}

// ErrUsage is returned when the caller supplied an invalid subcommand or
// flag set; the help banner has already been printed.
var ErrUsage = errors.New("usage")

// app encapsulates the CLI so it stays testable: tests construct an app, point
// it at an httptest URL and assert on stdout/stderr.
type app struct {
	out  io.Writer
	err  io.Writer
	args []string
}

func newApp(out, errOut io.Writer, args []string) *app {
	return &app{out: out, err: errOut, args: args}
}

func (a *app) run(ctx context.Context) error {
	root := flag.NewFlagSet("strata admin", flag.ContinueOnError)
	root.SetOutput(a.err)
	endpoint := root.String("endpoint", envOrDefault("STRATA_ADMIN_ENDPOINT", "http://localhost:9000"), "gateway endpoint URL")
	principal := root.String("principal", os.Getenv("STRATA_ADMIN_PRINCIPAL"), "X-Test-Principal header value (test harness shortcut)")
	jsonOut := root.Bool("json", false, "emit raw JSON instead of human-formatted output")
	root.Usage = func() {
		fmt.Fprintln(a.err, "usage: strata admin [global flags] <iam|lifecycle|gc|sse|replicate|bucket|rewrap|bench-gc|bench-lifecycle> <subcommand> [flags]\n  bucket subcommands: inspect | reshard\n  rewrap takes no subcommand: strata admin rewrap [--target-key-id ID] [--dry-run] [--batch N]\n  bench-gc / bench-lifecycle take no subcommand: strata admin bench-gc [--entries N] [--concurrency M]")
		root.PrintDefaults()
	}

	if err := root.Parse(a.args); err != nil {
		return ErrUsage
	}
	rest := root.Args()
	if len(rest) >= 1 && rest[0] == "rewrap" {
		return a.cmdRewrap(ctx, *jsonOut, rest[1:])
	}
	if len(rest) >= 1 && rest[0] == "bench-gc" {
		return a.cmdBenchGC(ctx, *jsonOut, rest[1:])
	}
	if len(rest) >= 1 && rest[0] == "bench-lifecycle" {
		return a.cmdBenchLifecycle(ctx, *jsonOut, rest[1:])
	}
	if len(rest) < 2 {
		root.Usage()
		return ErrUsage
	}

	client := &Client{Endpoint: *endpoint, Principal: *principal, UserAgent: "strata-admin/1"}
	group, sub := rest[0], rest[1]
	args := rest[2:]

	switch group + " " + sub {
	case "iam create-access-key":
		return a.cmdIAMCreateAccessKey(ctx, client, *jsonOut, args)
	case "iam rotate-access-key":
		return a.cmdIAMRotateAccessKey(ctx, client, *jsonOut, args)
	case "lifecycle tick":
		return a.cmdLifecycleTick(ctx, client, *jsonOut, args)
	case "gc drain":
		return a.cmdGCDrain(ctx, client, *jsonOut, args)
	case "sse rotate":
		return a.cmdSSERotate(ctx, client, *jsonOut, args)
	case "replicate retry":
		return a.cmdReplicateRetry(ctx, client, *jsonOut, args)
	case "bucket inspect":
		return a.cmdBucketInspect(ctx, client, *jsonOut, args)
	case "bucket reshard":
		return a.cmdBucketReshard(ctx, client, *jsonOut, args)
	default:
		fmt.Fprintf(a.err, "unknown command: %s %s\n", group, sub)
		root.Usage()
		return ErrUsage
	}
}

func (a *app) cmdIAMCreateAccessKey(ctx context.Context, c *Client, jsonOut bool, args []string) error {
	fs := flag.NewFlagSet("iam create-access-key", flag.ContinueOnError)
	fs.SetOutput(a.err)
	user := fs.String("user", "", "IAM user name")
	if err := fs.Parse(args); err != nil {
		return ErrUsage
	}
	if *user == "" {
		return errors.New("--user is required")
	}
	ak, err := c.CreateAccessKey(ctx, *user)
	if err != nil {
		return err
	}
	return a.emitAccessKey(ak, jsonOut)
}

func (a *app) cmdIAMRotateAccessKey(ctx context.Context, c *Client, jsonOut bool, args []string) error {
	fs := flag.NewFlagSet("iam rotate-access-key", flag.ContinueOnError)
	fs.SetOutput(a.err)
	id := fs.String("access-key-id", "", "the access key id to rotate")
	if err := fs.Parse(args); err != nil {
		return ErrUsage
	}
	if *id == "" {
		return errors.New("--access-key-id is required")
	}
	ak, err := c.RotateAccessKey(ctx, *id)
	if err != nil {
		return err
	}
	return a.emitAccessKey(ak, jsonOut)
}

func (a *app) emitAccessKey(ak *AccessKey, jsonOut bool) error {
	if jsonOut {
		return writeJSON(a.out, ak)
	}
	fmt.Fprintf(a.out, "user:        %s\n", ak.UserName)
	fmt.Fprintf(a.out, "access_key:  %s\n", ak.AccessKeyID)
	if ak.SecretAccessKey != "" {
		fmt.Fprintf(a.out, "secret_key:  %s\n", ak.SecretAccessKey)
	}
	fmt.Fprintf(a.out, "status:      %s\n", ak.Status)
	fmt.Fprintf(a.out, "created:     %s\n", ak.CreateDate)
	return nil
}

func (a *app) cmdLifecycleTick(ctx context.Context, c *Client, jsonOut bool, args []string) error {
	fs := flag.NewFlagSet("lifecycle tick", flag.ContinueOnError)
	fs.SetOutput(a.err)
	if err := fs.Parse(args); err != nil {
		return ErrUsage
	}
	res, err := c.LifecycleTick(ctx)
	if err != nil {
		return err
	}
	if jsonOut {
		return writeJSON(a.out, res)
	}
	fmt.Fprintf(a.out, "lifecycle: ok=%t duration=%dms\n", res.OK, res.DurationMs)
	if res.Error != "" {
		fmt.Fprintf(a.out, "error:     %s\n", res.Error)
	}
	return nil
}

func (a *app) cmdGCDrain(ctx context.Context, c *Client, jsonOut bool, args []string) error {
	fs := flag.NewFlagSet("gc drain", flag.ContinueOnError)
	fs.SetOutput(a.err)
	if err := fs.Parse(args); err != nil {
		return ErrUsage
	}
	res, err := c.GCDrain(ctx)
	if err != nil {
		return err
	}
	if jsonOut {
		return writeJSON(a.out, res)
	}
	fmt.Fprintf(a.out, "gc: drained=%d duration=%dms\n", res.Drained, res.DurationMs)
	return nil
}

func (a *app) cmdSSERotate(ctx context.Context, c *Client, jsonOut bool, args []string) error {
	fs := flag.NewFlagSet("sse rotate", flag.ContinueOnError)
	fs.SetOutput(a.err)
	if err := fs.Parse(args); err != nil {
		return ErrUsage
	}
	res, err := c.SSERotate(ctx)
	if err != nil {
		return err
	}
	if jsonOut {
		return writeJSON(a.out, res)
	}
	fmt.Fprintf(a.out, "sse rotate: ok=%t active=%s\n", res.OK, res.ActiveID)
	fmt.Fprintf(a.out, "  buckets:  scanned=%d skipped=%d\n", res.BucketsScanned, res.BucketsSkipped)
	fmt.Fprintf(a.out, "  objects:  scanned=%d rewrapped=%d\n", res.ObjectsScanned, res.ObjectsRewrapped)
	fmt.Fprintf(a.out, "  uploads:  scanned=%d rewrapped=%d\n", res.UploadsScanned, res.UploadsRewrapped)
	if res.Error != "" {
		fmt.Fprintf(a.out, "  error:    %s\n", res.Error)
	}
	return nil
}

func (a *app) cmdReplicateRetry(ctx context.Context, c *Client, jsonOut bool, args []string) error {
	fs := flag.NewFlagSet("replicate retry", flag.ContinueOnError)
	fs.SetOutput(a.err)
	bucket := fs.String("bucket", "", "bucket whose FAILED replication rows should be re-emitted")
	if err := fs.Parse(args); err != nil {
		return ErrUsage
	}
	if *bucket == "" {
		return errors.New("--bucket is required")
	}
	res, err := c.ReplicateRetry(ctx, *bucket)
	if err != nil {
		return err
	}
	if jsonOut {
		return writeJSON(a.out, res)
	}
	fmt.Fprintf(a.out, "replicate retry: bucket=%s scanned=%d requeued=%d\n", res.Bucket, res.Scanned, res.Requeued)
	if res.Error != "" {
		fmt.Fprintf(a.out, "  error: %s\n", res.Error)
	}
	return nil
}

func (a *app) cmdBucketInspect(ctx context.Context, c *Client, jsonOut bool, args []string) error {
	fs := flag.NewFlagSet("bucket inspect", flag.ContinueOnError)
	fs.SetOutput(a.err)
	bucket := fs.String("bucket", "", "bucket name")
	if err := fs.Parse(args); err != nil {
		return ErrUsage
	}
	if *bucket == "" {
		return errors.New("--bucket is required")
	}
	res, err := c.BucketInspect(ctx, *bucket)
	if err != nil {
		return err
	}
	if jsonOut {
		return writeJSON(a.out, res)
	}
	fmt.Fprintf(a.out, "bucket:      %s\n", res.Name)
	fmt.Fprintf(a.out, "id:          %s\n", res.ID)
	fmt.Fprintf(a.out, "owner:       %s\n", res.Owner)
	fmt.Fprintf(a.out, "created_at:  %s\n", res.CreatedAt.Format("2006-01-02T15:04:05Z07:00"))
	fmt.Fprintf(a.out, "default:     %s\n", res.DefaultClass)
	if res.Versioning != "" {
		fmt.Fprintf(a.out, "versioning:  %s\n", res.Versioning)
	}
	if res.Region != "" {
		fmt.Fprintf(a.out, "region:      %s\n", res.Region)
	}
	if res.MfaDelete != "" {
		fmt.Fprintf(a.out, "mfa_delete:  %s\n", res.MfaDelete)
	}
	fmt.Fprintf(a.out, "lock:        %t\n", res.ObjectLockEnabled)
	if len(res.Configs) > 0 {
		fmt.Fprintf(a.out, "configs:\n")
		for name := range res.Configs {
			fmt.Fprintf(a.out, "  - %s\n", name)
		}
	}
	return nil
}

func (a *app) cmdBucketReshard(ctx context.Context, c *Client, jsonOut bool, args []string) error {
	fs := flag.NewFlagSet("bucket reshard", flag.ContinueOnError)
	fs.SetOutput(a.err)
	bucket := fs.String("bucket", "", "bucket name")
	target := fs.Int("target", 0, "target shard count (positive power of two, larger than current)")
	if err := fs.Parse(args); err != nil {
		return ErrUsage
	}
	if *bucket == "" {
		return errors.New("--bucket is required")
	}
	if *target <= 0 {
		return errors.New("--target is required")
	}
	res, err := c.BucketReshard(ctx, *bucket, *target)
	if err != nil {
		return err
	}
	if jsonOut {
		return writeJSON(a.out, res)
	}
	fmt.Fprintf(a.out, "bucket reshard: bucket=%s source=%d target=%d\n", res.Bucket, res.Source, res.Target)
	fmt.Fprintf(a.out, "  jobs:    scanned=%d completed=%d\n", res.JobsScanned, res.JobsCompleted)
	fmt.Fprintf(a.out, "  objects: copied=%d\n", res.ObjectsCopied)
	if res.Error != "" {
		fmt.Fprintf(a.out, "  error:   %s\n", res.Error)
	}
	return nil
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
