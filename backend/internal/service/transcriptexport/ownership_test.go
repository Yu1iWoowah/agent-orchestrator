package transcriptexport

import (
	"errors"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestVerifyNativeTurnOwnership(t *testing.T) {
	for _, test := range []struct {
		name    string
		wantErr bool
	}{
		{"main", false}, {"missing", true}, {"conflict", true}, {"missing branch", true},
		{"old owned epoch", false}, {"confirmed child", false}, {"unconfirmed child", true},
		{"batch member", false}, {"active turn", false}, {"AO only", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, _, _ := sourceFixture()
			s.Branches = []domain.ConversationBranch{{ID: "b", ProviderConversationID: "epoch2"}}
			s.Turns = []domain.ConversationTurn{{ID: "turn", ConversationID: "c", BranchID: "b", ProviderTurnID: "prompt", State: domain.TurnStateCompleted}}
			s.NativeThreads = []domain.ConversationNativeThreadEvidence{{BranchID: "b", ProviderTurnID: "prompt", ProviderConversationID: "epoch2"}}
			switch test.name {
			case "missing":
				s.NativeThreads = nil
			case "conflict":
				s.NativeThreads = append(s.NativeThreads, domain.ConversationNativeThreadEvidence{BranchID: "b", ProviderTurnID: "prompt", ProviderConversationID: "other"})
			case "missing branch":
				s.Branches = nil
			case "old owned epoch":
				s.NativeThreads[0].ProviderConversationID = "epoch1"
			case "confirmed child", "unconfirmed child":
				s.NativeThreads[0].ProviderConversationID = "child"
				if test.name == "confirmed child" {
					s.Bindings = []domain.UsageBindingRecord{{ID: 1, Harness: domain.HarnessClaudeCode, NativeRootID: "epoch2"}}
					s.Sources = []domain.UsageSourceRecord{{BindingID: 1, Kind: domain.UsageSourceClaudeSubagent, NativeSessionID: "epoch2", SubagentID: "child"}}
				}
			case "batch member":
				s.Turns = append(s.Turns, domain.ConversationTurn{ID: "member", ConversationID: "c", BranchID: "b", ProviderTurnID: "unowned-member", State: domain.TurnStateCompleted})
				s.Inputs = []domain.TranscriptTurnInput{{TurnID: "member", PromotedToTurnID: "turn", BatchedInput: true}}
			case "active turn":
				s.Turns[0].State = domain.TurnStateRunning
				s.NativeThreads = nil
			case "AO only":
				s.Turns[0].ProviderTurnID = ""
				s.NativeThreads = nil
			}
			err := VerifyNativeTurnOwnership(s)
			if test.wantErr {
				var native *ports.NativeTranscriptError
				if !errors.As(err, &native) || native.Code != "TRANSCRIPT_PROVENANCE_UNKNOWN" {
					t.Fatalf("error=%v, want ownership failure", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}
