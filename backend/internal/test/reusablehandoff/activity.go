//go:build linux

// Package reusablehandoff contains test-only process and transport fixtures.
package reusablehandoff

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/process"
)

// DirectiveInput supplies semantic messages to the real controller fixture.
type DirectiveInput struct {
	Rebatch  bool                    `json:"rebatch"`
	Active   bool                    `json:"active"`
	Messages []ports.ChatUserMessage `json:"messages"`
}

// Input is the raw command result before the existing driver normalizes it.
type Input struct {
	Directive *DirectiveInput `json:"directive,omitempty"`
	Command   string          `json:"command"`
	Stdout    string          `json:"stdout"`
	ExitCode  *int            `json:"exit_code"`
	Actor     string          `json:"actor"`
	Turn      string          `json:"turn"`
	ID        string          `json:"id"`
	Sequence  int64           `json:"sequence"`
	Snapshot  json.RawMessage `json:"snapshot"`
}

// Repository finds the checkout for the isolated fixture binaries.
func Repository(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "backend", "go.mod")); err == nil {
			return dir
		}
		next := filepath.Dir(dir)
		if next == dir {
			t.Fatal("repo missing")
		}
		dir = next
	}
}

// BuildCLI builds only the test projector, which delegates CLI work to Cobra.
func BuildCLI(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "projection-cli")
	cmd := process.Command("go", "build", "-o", bin, "./internal/test/reusablehandoff/projection")
	cmd.Dir = filepath.Join(Repository(t), "backend")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fixture CLI: %v\n%s", err, out)
	}
	return bin
}

// RunSuite feeds actual Python helper results through a real driver and CLI.
func RunSuite(t *testing.T, normalize func(Input) ports.ChatEvent) {
	t.Helper()
	zero := 0
	sample := Input{Command: "python3 -I -B /synthetic/handoff.py inspect --run 00000000-0000-4000-8000-000000000001 --path start.json --offset 0", Stdout: `{"lease":{"session_id":"old"},"prepared_by":{"session_id":"old"}}`, ExitCode: &zero, Actor: "worker", Turn: "turn", ID: "tool", Sequence: 1}
	event, err := json.MarshalIndent(normalize(sample), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	event = append(event, '\n')
	driver := filepath.Base(func() string { d, _ := os.Getwd(); return d }())
	golden := filepath.Join(Repository(t), "tools", "reusable-handoff", "tests", "fixtures", "activities", driver+".json")
	if os.Getenv("RH_UPDATE_ACTIVITIES") == "1" {
		if err := os.MkdirAll(filepath.Dir(golden), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, event, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	expected, err := os.ReadFile(golden)
	if err != nil || !bytes.Equal(expected, event) {
		t.Fatalf("producer activity drift (%s); inspect producer then explicitly RH_UPDATE_ACTIVITIES=1: %v", golden, err)
	}
	bin := BuildCLI(t)
	fixtures := t.TempDir()
	cmd := process.Command(bin, "--rh-fixtures", fixtures)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Cobra fixtures: %v\n%s", err, out)
	}
	projections := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input Input
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var event ports.ChatEvent
		if input.Directive == nil {
			event = normalize(input)
		}
		raw, err := json.Marshal(struct {
			Input Input
			Event ports.ChatEvent
		}{input, event})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		dir, err := os.MkdirTemp(projections, "p-")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer func() {
			if err := os.RemoveAll(dir); err != nil {
				t.Errorf("remove projector scratch: %v", err)
			}
		}()
		action := "--rh-project"
		if input.Directive != nil {
			action = "--rh-directive"
		}
		cmd := process.Command(bin, action, dir)
		cmd.Stdin = bytes.NewReader(raw)
		out, err := cmd.CombinedOutput()
		if err != nil {
			http.Error(w, string(out), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
	}))
	defer server.Close()
	tests := filepath.Join(Repository(t), "tools", "reusable-handoff", "tests")
	// Include the actual repository-tree and retirement proofs in each driver suite.
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Second)
	defer cancel()
	cmd = process.CommandContext(ctx, "python3", "-I", "-B", "-m", "unittest", "discover", "-v", "-s", tests)
	cmd.Env = append(os.Environ(), "RH_WIRE_FIXTURES="+fixtures, "RH_ACTIVITY_PRODUCER="+server.URL)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err = cmd.Run()
	if err != nil {
		t.Fatalf("RH producer chain: %v", err)
	}
}
