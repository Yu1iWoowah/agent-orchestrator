package lifecycle

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestFinalizeReplacementUsagePreservesSessionsAndContainers(t *testing.T) {
	for _, change := range []string{"current", "stale revision", "rotated owner", "rotated during finalization", "best effort usage failure", "cancelled"} {
		t.Run(change, func(t *testing.T) {
			reaper := &fakeLifecycleContainerReaper{removed: 2}
			m, store, _ := newManagerWithContainerReaper(reaper, nil)
			source := working("mer-source")
			source.Kind, source.Harness, source.Mode = domain.KindOrchestrator, domain.HarnessClaudeCode, domain.SessionModeChat
			source.Metadata.ControllerGeneration = "generation"
			source.Metadata.ProviderConversationID = "native-source"
			source.Metadata.BrowserCapabilityVerifier = "verifier"
			store.sessions[source.ID] = source
			worker := working("mer-worker")
			store.sessions[worker.ID] = worker
			finalizer := &fakeUsageFinalizer{store: store}
			m.SetUsageFinalizer(finalizer)
			callContext := context.Background()
			switch change {
			case "stale revision":
				source.Revision--
			case "rotated owner":
				source.Metadata.ControllerGeneration = "old-generation"
			case "rotated during finalization":
				finalizer.onFinalize = func(id domain.SessionID, _ string, _ int64) error {
					rec := store.sessions[id]
					rec.Metadata.ControllerGeneration = "rotated"
					store.sessions[id] = rec
					return nil
				}
			case "best effort usage failure":
				finalizer.err = errors.New("usage unavailable")
			case "cancelled":
				cancelled, cancel := context.WithCancel(callContext)
				cancel()
				callContext = cancelled
			}
			err := m.FinalizeReplacementUsage(callContext, source)
			wantSuccess := change == "current" || change == "best effort usage failure"
			if (err == nil) != wantSuccess {
				t.Fatalf("%s: err=%v", change, err)
			}
			if change == "stale revision" || change == "rotated owner" || change == "cancelled" {
				if finalizer.calls != 0 {
					t.Fatal("stale/cancelled source reached usage finalizer")
				}
			} else if finalizer.calls != 1 || finalizer.sawTerminated || finalizer.launchID != source.Metadata.RuntimeLaunchID || finalizer.sessionRevision != source.Revision {
				t.Fatalf("usage was not finalized before retirement: %+v", finalizer)
			}
			if store.sessions[source.ID].IsTerminated || len(reaper.sessions) != 0 || !reflect.DeepEqual(store.sessions[worker.ID], worker) {
				t.Fatal("usage preparation retired a session or reaped containers")
			}
			if wantSuccess && !reflect.DeepEqual(store.sessions[source.ID], source) {
				t.Fatal("usage preparation changed source facts")
			}
		})
	}
}
