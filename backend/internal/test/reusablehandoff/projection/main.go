//go:build linux

// This executable is a test adapter, never part of the AO distribution.
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/aoagents/agent-orchestrator/backend/internal/cli"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	rh "github.com/aoagents/agent-orchestrator/backend/internal/test/reusablehandoff"
)

type checks struct{ root string }

func (t *checks) Helper()           {}
func (t *checks) Fatal(args ...any) { fmt.Fprintln(os.Stderr, args...); os.Exit(1) }
func (t *checks) Fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format, args...)
	os.Exit(1)
}
func (t *checks) TempDir() string {
	dir, err := os.MkdirTemp(t.root, "rh-")
	if err != nil {
		t.Fatal(err)
	}
	return dir
}
func main() {
	bin, err := os.Executable()
	if err != nil {
		panic(err)
	}
	if len(os.Args) == 3 && os.Args[1] == "--rh-fixtures" {
		Fixtures(&checks{os.Args[2]}, bin, os.Args[2])
		return
	}
	if len(os.Args) == 3 && os.Args[1] == "--rh-project" {
		t := &checks{os.Args[2]}
		var request struct {
			Input rh.Input
			Event ports.ChatEvent
		}
		if err := json.NewDecoder(os.Stdin).Decode(&request); err != nil {
			t.Fatal(err)
		}
		_, _ = os.Stdout.Write(project(t, bin, request.Input, request.Event))
		return
	}
	cmd := cli.NewRootCommand(cli.DefaultDeps())
	cmd.SetArgs(os.Args[1:])
	if err := cmd.Execute(); err != nil {
		os.Exit(cli.ExitCode(err))
	}
}
