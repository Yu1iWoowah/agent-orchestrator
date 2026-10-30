package tmux

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestStopSessionRuntimesRejectsEmptyID(t *testing.T) {
	r, fr := newTestRuntime(0)
	if err := r.StopSessionRuntimes(context.Background(), ""); !errors.Is(err, ports.ErrRuntimeProbeInconclusive) {
		t.Fatalf("empty namespace was not rejected: %v", err)
	}
	if len(fr.calls) != 0 {
		t.Fatal("empty namespace reached tmux")
	}
}

func TestStopSessionRuntimesKeepsUncertainFailuresRetryable(t *testing.T) {
	for _, failure := range []string{"set-option", "kill-session"} {
		t.Run(failure, func(t *testing.T) {
			r, _ := newTestRuntime(0)
			id := domain.SessionID(strings.Repeat("project-", 8) + "1")
			name := SessionName(string(id))
			fr := &fakeRunnerSelectiveErr{exitErrOn: failure, errOutput: []byte("connection refused")}
			r.runner = fr
			r.rememberSessionSocket(name, r.socketName)
			if err := r.StopSessionRuntimes(context.Background(), id); err == nil {
				t.Fatalf("%s failure was treated as confirmed shutdown", failure)
			}
			if failure != "kill-session" {
				for _, call := range fr.calls {
					if len(call.args) > 0 && call.args[0] == "kill-session" {
						t.Fatal("uncertain probe/guard allowed shutdown")
					}
				}
			}
			// Retry must still address the normalized private namespace.
			r.runner = &fakeRunner{}
			if err := r.StopSessionRuntimes(context.Background(), id); err != nil {
				t.Fatalf("confirmed retry failed: %v", err)
			}
		})
	}
}

func TestStopSessionRuntimesDoesNotTurnConnectionUncertaintyIntoAbsence(t *testing.T) {
	r, fr := newTestRuntime(0)
	r.socketName = "private-shutdown"
	fr.err = errors.New("connection refused")
	if err := r.StopSessionRuntimes(context.Background(), "session-1"); err == nil {
		t.Fatal("inconclusive private probes/commands were treated as absence")
	}
	for _, call := range fr.calls {
		if len(call.args) < 2 || call.args[0] != "-L" || call.args[1] != r.socketName {
			t.Fatalf("uncertainty escaped the private namespace: %+v", call)
		}
	}
	fr.err = nil
	if err := r.StopSessionRuntimes(context.Background(), "session-1"); err != nil {
		t.Fatalf("confirmed retry failed: %v", err)
	}
}
