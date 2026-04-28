package main

import (
	"flag"
	"fmt"
	"runtime"
	"runtime/debug"
)

// runVersion prints the build's git SHA and Go runtime version. The SHA comes
// from runtime/debug.BuildInfo (Go's vcs.* settings, populated automatically
// when building from a git checkout); when unavailable (e.g. `go run` with no
// VCS context) we fall back to the literal "unknown".
func (a *app) runVersion(args []string) int {
	fs := flag.NewFlagSet("strata version", flag.ContinueOnError)
	fs.SetOutput(a.err)
	fs.Usage = func() {
		fmt.Fprintln(a.out, "usage: strata version")
		fmt.Fprintln(a.out)
		fmt.Fprintln(a.out, "Prints the build's git SHA and Go runtime.")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	fmt.Fprintf(a.out, "strata sha=%s runtime=%s\n", buildSHA(), runtime.Version())
	return 0
}

func buildSHA() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" && s.Value != "" {
			return s.Value
		}
	}
	return "unknown"
}
