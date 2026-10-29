package sessionmanager

import (
	"context"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestHumanAnswerSemanticDeliveryWaitsForExactAcceptanceAndDeduplicates(t *testing.T) {
	const key = "question-answer-ask-1"
	store := newFakeStore()
	store.sessions["s1"] = pastStartupGate(domain.SessionRecord{ID: "s1", Harness: domain.HarnessClaudeCode, Mode: domain.SessionModeTUI, Activity: domain.Activity{State: domain.ActivityIdle}, Metadata: domain.SessionMetadata{RuntimeLaunchID: "launch-1"}})
	messenger := &fakeMessenger{onSend: func(id domain.SessionID, message string) {
		if got, ok := domain.ReportDeliveryID(message); !ok || got != key {
			t.Fatalf("semantic envelope = %q %v", got, ok)
		}
		rec := store.sessions[id]
		rec.Activity.State = domain.ActivityActive
		rec.Metadata.ConversationCheckpointState = domain.ConversationCheckpointCoordination
		rec.Metadata.ConversationCheckpointGeneration = "launch-1"
		rec.Metadata.ConversationCheckpointTurnID = key
		store.sessions[id] = rec
	}}
	manager := newSendTestManager(t, semanticSignalingAgent{}, messenger, store)
	for range 2 {
		if err := manager.SendSemanticWithOptions(context.Background(), "s1", "User chose Staging", key, ports.MessageDeliveryOptions{AuthoredByUser: true}); err != nil {
			t.Fatal(err)
		}
	}
	if len(messenger.msgs) != 1 || !semanticMessageAccepted(store.sessions["s1"], key) {
		t.Fatalf("human semantic delivery = %+v; checkpoint=%+v", messenger.msgs, store.sessions["s1"].Metadata)
	}
}
