package sessionmanager

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestReplacementSuccessorPollingAcceptsBackgroundRevisionProgress(t *testing.T) {
	for _, boundary := range []string{"launch", "controller commit"} {
		t.Run(boundary, func(t *testing.T) {
			m, store, chat, deferred := replacementSuccessorFixture(t)
			ctx := context.Background()
			seed, err := m.EnsureReplacementSuccessor(ctx, store.operation)
			if err != nil {
				t.Fatal(err)
			}
			expected := store.operation
			polled := false
			poll := func() {
				polled = true
				if store.operation.Revision <= expected.Revision {
					t.Fatal("background receipt did not advance revision")
				}
				observed, err := m.EnsureReplacementSuccessor(ctx, expected)
				if err != nil || observed.ID != seed.ID || store.operation.Phase != domain.ReplacementStarting {
					t.Fatal("ordinary background progress was treated as ownership loss", observed.ID, err)
				}
			}
			if boundary == "launch" {
				chat.beforeStart = func(ChatStart) { poll() }
			} else {
				chat.afterReady = func() {
					rec := store.sessions[seed.ID]
					if rec.Metadata.ProviderConversationID == "" || rec.Metadata.ControllerGeneration == "" {
						t.Fatal("controller callback did not publish native identity")
					}
					// Match SQLite's same-transaction controller receipt.
					store.operation.SuccessorController = &domain.ReplacementSuccessorController{Owner: rec.ControllerOwner(), CapabilityVerifier: rec.Metadata.BrowserCapabilityVerifier, CommittedAt: time.Now()}
					store.operation.Revision++
					poll()
				}
			}
			(*deferred)[0]()
			ready, err := m.EnsureReplacementSuccessor(ctx, store.operation)
			if !polled || err != nil || ready.ID != seed.ID || ready.ProvisionState != domain.SessionProvisionReady || store.creates != 1 || len(chat.started) != 1 || len(*deferred) != 1 {
				t.Fatal("polling prevented the one controller from becoming ready", polled, ready.ProvisionState, err)
			}
		})
	}
}
