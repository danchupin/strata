// strata is the unified gateway+workers binary. The root command dispatches
// to subcommands; the worker registry and full server entrypoint land in
// follow-up stories (see scripts/ralph/prd.json US-003+).
package main

import (
	"context"
	"fmt"
	"io"
	"os"
)

func main() {
	app := newApp(os.Stdout, os.Stderr, os.Args[1:])
	code := app.run(context.Background())
	os.Exit(code)
}

type app struct {
	out  io.Writer
	err  io.Writer
	args []string
}

func newApp(out, errOut io.Writer, args []string) *app {
	return &app{out: out, err: errOut, args: args}
}

func (a *app) run(ctx context.Context) int {
	if len(a.args) == 0 {
		a.printRootHelp()
		return 0
	}
	switch a.args[0] {
	case "-h", "--help", "help":
		a.printRootHelp()
		return 0
	case "version":
		return a.runVersion(a.args[1:])
	case "server":
		return a.runServer(ctx, a.args[1:])
	default:
		fmt.Fprintf(a.err, "strata: unknown subcommand %q\n\n", a.args[0])
		a.printRootHelp()
		return 2
	}
}

func (a *app) printRootHelp() {
	fmt.Fprintln(a.out, "strata — unified S3 gateway + background workers")
	fmt.Fprintln(a.out)
	fmt.Fprintln(a.out, "usage: strata <subcommand> [flags]")
	fmt.Fprintln(a.out)
	fmt.Fprintln(a.out, "subcommands:")
	fmt.Fprintln(a.out, "  server     run the S3 gateway (and optional background workers)")
	fmt.Fprintln(a.out, "  version    print build version (git SHA) and Go runtime")
	fmt.Fprintln(a.out, "  help       print this help")
	fmt.Fprintln(a.out)
	fmt.Fprintln(a.out, "run `strata <subcommand> --help` for subcommand flags.")
}
