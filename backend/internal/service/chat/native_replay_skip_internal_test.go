package chat

import (
	"errors"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// A replayed settlement is skipped only when it would write AO's current state
// under the store's own keep-failure rule (SDR-PR1 AC-R4).
func TestSkipSettledReplayFollowsTheWrittenState(t *testing.T) {
	for _, tc := range []struct {
		name     string
		stored   domain.ConversationTurn
		replayed ports.ChatEvent
		skipped  bool
	}{
		{"AO turn still running", domain.ConversationTurn{State: domain.TurnStateRunning},
			ports.ChatEvent{TurnState: domain.TurnStateCompleted}, false},
		{"recovered upgraded by a known outcome", domain.ConversationTurn{State: domain.TurnStateRecovered},
			ports.ChatEvent{TurnState: domain.TurnStateCompleted}, false},
		{"Codex failed turn replayed completed", domain.ConversationTurn{State: domain.TurnStateFailed, ErrorMessage: "usage limit"},
			ports.ChatEvent{TurnState: domain.TurnStateCompleted}, false},
		{"Codex failed turn replayed interrupted keeps its failure", domain.ConversationTurn{State: domain.TurnStateFailed, ErrorMessage: "usage limit", FailureCode: domain.ConversationFailureAuthRequired},
			ports.ChatEvent{TurnState: domain.TurnStateInterrupted}, true},
		{"failed turn replayed with a new error", domain.ConversationTurn{State: domain.TurnStateFailed, ErrorMessage: "usage limit"},
			ports.ChatEvent{TurnState: domain.TurnStateFailed, Err: errors.New("other")}, true},
		{"completed turn replayed completed", domain.ConversationTurn{State: domain.TurnStateCompleted},
			ports.ChatEvent{TurnState: domain.TurnStateCompleted}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.stored.ProviderTurnID = "native-turn"
			tc.replayed.Kind, tc.replayed.ProviderTurnID = ports.ChatEventTurnCompleted, "native-turn"
			events := []ports.ChatEvent{{Kind: ports.ChatEventTurnStarted, ProviderTurnID: "native-turn"}, tc.replayed}
			kept, skippedEvents, skippedTurns := skipSettledReplay(events, []domain.ConversationTurn{tc.stored}, nil)
			if skipped := skippedTurns == 1; skipped != tc.skipped {
				t.Fatalf("skipped=%v (events %d, kept %d), want %v", skipped, skippedEvents, len(kept), tc.skipped)
			}
			if tc.skipped && len(kept) != 0 {
				t.Fatalf("kept %d lifecycle events of a skipped turn", len(kept))
			}
		})
	}
}
