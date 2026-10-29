package chat

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/session_manager"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// Hide the optional preflight to model acceptance between that read and Manager
// admission. The callback still exercises the real Manager and durable fence.
type replacementWakeSessionReader struct{ SessionReader }

func TestReplacementWakeUsesManagerFenceWithoutFailingQueuedInput(t *testing.T) {
	for _, path := range []string{"preflight", "view admission race", "background admission race"} {
		t.Run(path, func(t *testing.T) {
			ctx := context.Background()
			st := sqlitetest.MustOpenAt(t, t.TempDir())
			now := time.Now().UTC()
			if err := st.UpsertProject(ctx, domain.ProjectRecord{ID: "wake-project", Path: t.TempDir(), RegisteredAt: now}); err != nil {
				t.Fatal(err)
			}
			source, err := st.CreateSession(ctx, domain.SessionRecord{ProjectID: "wake-project", Kind: domain.KindOrchestrator, Harness: domain.HarnessClaudeCode, Mode: domain.SessionModeChat, HibernatedAt: &now, CreatedAt: now, UpdatedAt: now, Metadata: domain.SessionMetadata{ControllerGeneration: "generation", ProviderConversationID: "native", BrowserCapabilityVerifier: "capability"}})
			if err != nil {
				t.Fatal(err)
			}
			if applied, err := st.SetSessionHibernated(ctx, source.ID, source.Revision, &now); err != nil || !applied {
				t.Fatalf("hibernate fixture: %t %v", applied, err)
			}
			source, _, err = st.GetSession(ctx, source.ID)
			if err != nil || source.HibernatedAt == nil {
				t.Fatalf("read hibernated fixture: %v", err)
			}
			op, _, err := st.CreateOrchestratorReplacement(ctx, domain.OrchestratorReplacement{SchemaVersion: 1, ID: "11111111-1111-4111-8111-111111111111", OperationKey: "22222222-2222-4222-8222-222222222222", RequestHash: strings.Repeat("a", 64), ProjectID: source.ProjectID, SourceSessionID: source.ID, SourceOwner: source.ControllerOwner(), SourceCapabilityVerifier: source.Metadata.BrowserCapabilityVerifier, Phase: domain.ReplacementAccepted, CreatedAt: now, UpdatedAt: now, DeadlineAt: now.Add(time.Hour)})
			if err != nil {
				t.Fatal(err)
			}
			next := 0
			svc := New(Options{Store: st, Sessions: st, NewID: func() string { next++; return fmt.Sprintf("wake-%d", next) }})
			turn, err := svc.QueueReplacementIntake(ctx, source.ID, ports.ChatUserMessage{Text: "keep queued", ClientMessageID: "queued"})
			if err != nil {
				t.Fatal(err)
			}
			manager := sessionmanager.New(sessionmanager.Deps{Store: st})
			calls := 0
			svc.SetWakeCallback(func(ctx context.Context, id domain.SessionID) error {
				calls++
				err := manager.WakeHibernatedChat(ctx, id)
				if !errors.Is(err, domain.ErrReplacementInProgress) {
					t.Errorf("unexpected real Manager error: %v", err)
				}
				// Once the op is cancelled, an erroneous failQueuedTurns is no
				// longer masked by the held-turn reader.
				if _, applied, cancelErr := st.ResetReplacementBeforeStop(ctx, op.ID, op.Revision, true, now, now.Add(time.Hour)); cancelErr != nil || !applied {
					t.Errorf("cancel fixture: %t %v", applied, cancelErr)
				}
				return err
			})
			if path != "preflight" {
				svc.sessions = replacementWakeSessionReader{st}
			}
			if path == "background admission race" {
				svc.startBackgroundWake(source.ID)
				deadline := time.Now().Add(5 * time.Second)
				for {
					svc.wakeMu.Lock()
					running := svc.backgroundWakes[source.ID]
					svc.wakeMu.Unlock()
					if !running {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("background wake did not finish")
					}
					time.Sleep(time.Millisecond)
				}
			} else if err := svc.SetChatView(ctx, source.ID, "view", true); err != nil {
				t.Errorf("fenced view returned an error: %v", err)
			}
			wantCalls := 1
			if path == "preflight" {
				wantCalls = 0
			}
			if calls != wantCalls {
				t.Errorf("provider admission calls=%d want=%d", calls, wantCalls)
			}
			stored, err := st.TurnByID(ctx, turn.ID)
			if err != nil || stored.State != domain.TurnStateQueued || stored.ProviderTurnID != "" {
				t.Fatalf("fenced wake damaged accepted input: state=%s err=%v", stored.State, err)
			}
		})
	}
}
