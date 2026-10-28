package chat_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestRetryTurnDoesNotRenderCapturedFileAgain(t *testing.T) {
	for _, withResource := range []bool{false, true} {
		name := "file only"
		if withResource {
			name = "file and resource"
		}
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			ctx := context.Background()
			caps := productionCaps()
			caps[ports.ChatCapabilityEmbeddedContext] = true
			h.conv.setCapabilities(caps)

			line := 1
			ref := ports.ChatFileReference{
				ID: "capture", SessionID: string(testSession), Path: "retry.ts",
				Source: ports.ChatFileReferenceSource{Kind: "workspace"},
				Side:   "file", Line: 1, EndLine: 1,
				Lines: []ports.ChatFileReferenceLine{{Kind: "context", NewNo: &line, Text: "RETRY_CAPTURE_MARKER"}},
			}
			content := []ports.ChatContent{{Type: "file_reference", FileReference: &ref}}
			resource := ports.ChatContent{Type: "resource", URI: "file:///worktree/notes.txt", Name: "notes.txt"}
			if withResource {
				content = append(content, resource)
			}
			original, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{
				Text: "explain the captured code", ClientMessageID: "retry-capture",
				Origin: domain.MessageOriginHuman, Content: content,
			})
			if err != nil {
				t.Fatal(err)
			}
			firstInput := h.conv.sentMessages()[0].Text
			if strings.Count(firstInput, "RETRY_CAPTURE_MARKER") != 1 {
				t.Fatalf("initial provider input = %q, want one capture", firstInput)
			}

			attempt := original
			for retry := 1; retry <= 2; retry++ {
				h.conv.emit(ports.ChatEvent{
					Kind: ports.ChatEventTurnCompleted, ProviderTurnID: attempt.ProviderTurnID,
					TurnState: domain.TurnStateFailed, Err: errors.New("provider disconnected"),
				})
				failedTurnSnapshot(t, h, attempt.ID)
				next, err := h.svc.RetryTurn(ctx, testSession, attempt.ID)
				if err != nil {
					t.Fatalf("retry %d: %v", retry, err)
				}
				sent := h.conv.sentMessages()
				if len(sent) != retry+1 {
					t.Fatalf("retry %d: got %d provider sends", retry, len(sent))
				}
				if got := sent[retry]; got.Text != firstInput {
					t.Fatalf("retry %d: provider input contains %d captures, want unchanged input with one capture", retry, strings.Count(got.Text, "RETRY_CAPTURE_MARKER"))
				} else if withResource && (len(got.Content) != 1 || !reflect.DeepEqual(got.Content[0], resource)) {
					t.Fatalf("retry %d lost resource: %+v", retry, got.Content)
				} else if !withResource && len(got.Content) != 0 {
					t.Fatalf("retry %d: unexpected provider content %+v", retry, got.Content)
				}

				snapshot, err := h.st.LoadConversationSnapshot(ctx, h.ctrl.ConversationID())
				if err != nil {
					t.Fatal(err)
				}
				linked, found := turnByID(snapshot, next.ID)
				if !found || linked.RetryOfTurnID != attempt.ID {
					t.Fatalf("retry source = %q, want %q", linked.RetryOfTurnID, attempt.ID)
				}
				var durable []ports.ChatContent
				for _, message := range snapshot.Messages {
					if message.TurnID == original.ID && message.Role == domain.MessageRoleUser {
						if err := json.Unmarshal([]byte(message.DeliveryContentJSON), &durable); err != nil {
							t.Fatal(err)
						}
					}
				}
				if len(durable) != len(content) || !reflect.DeepEqual(durable[0].FileReference, &ref) {
					t.Fatalf("retry %d lost durable capture provenance: %+v", retry, durable)
				}
				attempt = linked
			}
		})
	}
}
