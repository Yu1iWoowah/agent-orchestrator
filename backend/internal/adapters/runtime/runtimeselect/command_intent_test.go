package runtimeselect

import (
	"context"
	"errors"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type noCommandEffect struct{ error }

func (noCommandEffect) PossibleHandle() ports.RuntimeHandle       { return ports.RuntimeHandle{} }
func (noCommandEffect) EffectOutcome() ports.RuntimeEffectOutcome { return ports.RuntimeEffectNone }
func (noCommandEffect) CleanupOutcome() ports.RuntimeCleanupOutcome {
	return ports.RuntimeCleanupNotAttempted
}

type partialCommandFailure struct{ error }

func (partialCommandFailure) PossibleHandle() ports.RuntimeHandle {
	return ports.RuntimeHandle{ID: "shellterm-fake"}
}
func (partialCommandFailure) EffectOutcome() ports.RuntimeEffectOutcome {
	return ports.RuntimeEffectPossible
}
func (partialCommandFailure) CleanupOutcome() ports.RuntimeCleanupOutcome {
	return ports.RuntimeCleanupFailed
}

func TestTrustedCreatePersistsEachActualRouteAndRefusesUncertainFallback(t *testing.T) {
	for _, kind := range []string{"direct", "none", "partial", "intent-failure"} {
		t.Run(kind, func(t *testing.T) {
			direct := &fakeBackend{}
			legacy := &restartableFakeBackend{}
			if kind == "none" {
				direct.createErr = noCommandEffect{errors.New("fake startup error")}
			}
			if kind == "partial" {
				direct.createErr = partialCommandFailure{errors.New("fake partial startup")}
			}
			r := newHybridRuntime(legacy, direct, nil, "Linux")
			c, ok := any(r).(interface {
				CreateCommand(context.Context, ports.RuntimeConfig, func(ports.RuntimeHandle) error) (ports.RuntimeHandle, error)
			})
			if !ok {
				t.Fatal("runtime cannot durably reserve chosen trusted route")
			}
			var intents []string
			_, e := c.CreateCommand(context.Background(), ports.RuntimeConfig{SessionID: "shellterm-fake"}, func(h ports.RuntimeHandle) error {
				intents = append(intents, h.ID)
				if len(direct.calls) > 0 && len(intents) == 1 {
					t.Fatal("direct starts before intent")
				}
				if kind == "intent-failure" {
					return errors.New("disk full")
				}
				return nil
			})
			switch kind {
			case "none":
				if e != nil || len(intents) != 2 || len(legacy.calls) != 1 {
					t.Fatal("proven effect-free failure did not durably select fallback")
				}
			case "direct":
				if e != nil || len(intents) != 1 || len(legacy.calls) != 0 {
					t.Fatal("unused fallback reserved or started")
				}
			case "intent-failure":
				if e == nil || len(direct.calls) != 0 {
					t.Fatal("runtime started without intent")
				}
			case "partial":
				if e == nil || len(legacy.calls) != 0 {
					t.Fatal("uncertain direct create started fallback writer")
				}
			}
		})
	}
}
