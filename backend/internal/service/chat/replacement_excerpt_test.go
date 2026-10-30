package chat

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type replacementExcerptRows struct {
	Store
	allow        bool
	expectedHash string
	messages     []domain.ConversationMessage
	turn         domain.ConversationTurn
	reads        []string
}

func (s *replacementExcerptRows) ReplacementExcerptSourceAllowed(_ context.Context, session domain.SessionID, generation, target, turn, source, hash string) (bool, error) {
	return s.allow && session == "successor" && generation == "verified-generation" && target == "project-conversation" && turn == "queued-turn" && source == "original-source-conversation" && hash == s.expectedHash, nil
}
func (s *replacementExcerptRows) ConversationMessages(_ context.Context, id string) ([]domain.ConversationMessage, error) {
	s.reads = append(s.reads, id)
	return s.messages, nil
}
func (s *replacementExcerptRows) TurnByID(context.Context, string) (domain.ConversationTurn, error) {
	return s.turn, nil
}

func TestReplacementQueuedExcerptRequiresTransferAndRevalidatesOriginalSource(t *testing.T) {
	for _, fault := range []string{"verified", "no receipt", "payload changed", "source revision changed", "source rolled back"} {
		t.Run(fault, func(t *testing.T) {
			now := time.Now().UTC()
			reference := ports.ChatExcerptReference{ConversationID: "original-source-conversation", MessageID: "old-answer", Revision: 1, Text: "chosen section"}
			raw, err := json.Marshal([]ports.ChatContent{{Type: "image", Data: "aW1hZ2U=", MIMEType: "image/png"}, {Type: "excerpt", Excerpt: &ports.ChatExcerptContext{Reference: reference, UserMessage: "forged persisted pair", AssistantMessage: "forged persisted pair"}}})
			if err != nil {
				t.Fatal(err)
			}
			queued := domain.QueuedTurn{TurnID: "queued-turn", ClientMessageID: "original-client", Text: "keep this selection", Origin: domain.MessageOriginHuman, DeliveryContentJSON: string(raw)}
			hash, err := domain.ReplacementIntakePayloadHash(queued)
			if err != nil {
				t.Fatal(err)
			}
			rows := &replacementExcerptRows{allow: fault != "no receipt", expectedHash: hash, messages: []domain.ConversationMessage{{ID: "old-question", ConversationID: reference.ConversationID, TurnID: "source-completed-turn", Role: domain.MessageRoleUser, Text: "original question", Revision: 1}, {ID: "old-answer", ConversationID: reference.ConversationID, TurnID: "source-completed-turn", Role: domain.MessageRoleAssistant, Text: "historical answer: chosen section", Revision: 1}}, turn: domain.ConversationTurn{ID: "source-completed-turn", ConversationID: reference.ConversationID, State: domain.TurnStateCompleted, RequestedAt: now, CompletedAt: &now}}
			switch fault {
			case "payload changed":
				queued.Text = "changed after transfer"
			case "source revision changed":
				rows.messages[1].Revision = 2
			case "source rolled back":
				rows.turn.RolledBackAt = &now
			}
			controller := &Controller{sessionID: "successor", generation: "verified-generation", conversation: domain.ConversationRecord{ID: "project-conversation"}, store: rows}
			out, err := controller.queuedBatchInput(context.Background(), []domain.QueuedTurn{queued}, nil)
			if fault != "verified" {
				if err == nil {
					t.Fatal("unverified cross-source excerpt accepted", fault)
				}
				return
			}
			if err != nil {
				t.Fatal("verified relocated queue lost its original excerpt", err)
			}
			encoded, err := json.Marshal(struct {
				Text    string
				Content []ports.ChatContent
			}{out.Text, out.Content})
			if err != nil || !strings.Contains(string(encoded), "historical answer: chosen section") || !strings.Contains(string(encoded), "image/png") || strings.Contains(string(encoded), "forged persisted pair") {
				t.Fatal("excerpt source/payload was rewritten or trusted without reread", string(encoded), err)
			}
			if len(rows.reads) != 1 || rows.reads[0] != reference.ConversationID {
				t.Fatal("did not reread original transcript", rows.reads)
			}
			direct := ports.ChatUserMessage{Text: "foreign direct input", Excerpts: []ports.ChatExcerptReference{reference}}
			if err := hydrateExcerptReferences(context.Background(), controller, &direct); !errors.Is(err, ErrExcerptInvalid) {
				t.Fatal("public foreign excerpt bypassed normal boundary", err)
			}
		})
	}
}
