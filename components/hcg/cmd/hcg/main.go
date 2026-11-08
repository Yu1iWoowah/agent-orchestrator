// Command hcg reads AO usage through the daemon's public API.
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

	"github.com/aoagents/agent-orchestrator/components/hcg/internal/source"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) < 2 || args[0] != "usage" || (args[1] != "sync" && args[1] != "events" && args[1] != "current" && args[1] != "baseline") {
		fmt.Fprintln(stderr, "usage: hcg usage sync|events|current --session <id> [--json]; hcg usage baseline --sessions <ids> --from <UTC> --to <UTC> [--json]")
		return 2
	}
	if args[1] == "baseline" {
		return runBaseline(ctx, args[2:], stdout, stderr)
	}
	flags := flag.NewFlagSet("usage "+args[1], flag.ContinueOnError)
	flags.SetOutput(stderr)
	session := flags.String("session", os.Getenv("AO_SESSION_ID"), "AO session id")
	flags.Bool("json", false, "emit JSON (also the default)")
	if flags.Parse(args[2:]) != nil {
		return 2
	}
	if *session == "" || flags.NArg() != 0 {
		fmt.Fprintln(stderr, "a session id is required; use --session or AO_SESSION_ID")
		return 2
	}
	if args[1] == "current" {
		return runCurrent(ctx, *session, stdout, stderr)
	}
	client, err := source.FromEnvironment()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	snapshot, err := client.Scan(ctx, *session)
	return writeUsageResult(snapshot, err, stdout, stderr)
}
