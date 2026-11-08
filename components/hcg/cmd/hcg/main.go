// Command hcg reads AO usage through the daemon's public API.
package main

import (
	"context"
	"encoding/json"
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
	if len(args) < 2 || args[0] != "usage" || (args[1] != "sync" && args[1] != "events") {
		fmt.Fprintln(stderr, "usage: hcg usage sync|events --session <id> [--json]")
		return 2
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
	client, err := source.FromEnvironment()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	snapshot, err := client.Scan(ctx, *session)
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	if e := encoder.Encode(snapshot); e != nil {
		fmt.Fprintln(stderr, e)
		return 1
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}
