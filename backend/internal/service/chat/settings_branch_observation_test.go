package chat_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/agentsettings"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

type observedGrantEnvironment struct {
	ports.AgentSettingsGrantEnvironment
	mu    sync.Mutex
	token string
}

func (e *observedGrantEnvironment) AgentSettingsGrantEnv(ctx context.Context, id domain.SessionID, h domain.AgentHarness, m domain.SessionMode, g string) (map[string]string, error) {
	env, err := e.AgentSettingsGrantEnvironment.AgentSettingsGrantEnv(ctx, id, h, m, g)
	e.mu.Lock()
	e.token = env[ports.EnvAgentSettingsGrant]
	e.mu.Unlock()
	return env, err
}

func newObservedBranchHarness(t *testing.T) (*harness, *editDriverState, *agentsettings.Service) {
	t.Helper()
	h, history, driver := newEditHarnessWithStore(t, false, func(st *store.Store) chatsvc.Store {
		if err := st.BindSessionAgentAccount(context.Background(), testSession, domain.AgentAccountProviderCodex, "device-codex", time.Now()); err != nil {
			t.Fatal(err)
		}
		return st
	})
	at := h.clock
	receipt := domain.UnknownAgentSettingsEffective("provider_not_reported")
	receipt.ObservedAt = &at
	receipt.Source = "provider_config"
	receipt.Model = domain.AgentSettingObservation{State: domain.AgentSettingKnown, Value: "reported-model"}
	receipt.FastMode = domain.AgentSettingObservation{State: domain.AgentSettingNotProvided, Reason: "provider_not_offered"}
	all := []*fakeConversation{history.fakeConversation, driver.fresh}
	for _, c := range driver.resumed {
		all = append(all, c)
	}
	for _, c := range all {
		c.mu.Lock()
		c.settingsReceipt = &receipt
		c.mu.Unlock()
	}
	reader := agentsettings.New(h.st, func() time.Time { return h.clock })
	reader.SetObservationSource(h.svc)
	initial, err := reader.Read(context.Background(), testSession)
	if err != nil || initial.Effective.FastMode.State != domain.AgentSettingNotProvided {
		t.Fatalf("initial receipt: %+v %v", initial.Effective, err)
	}
	return h, driver, reader
}

func TestBranchReplacementRetainsQualifiedSettingsOwner(t *testing.T) {
	for _, action := range []string{"edit", "activate", "restore"} {
		t.Run(action, func(t *testing.T) {
			ctx := context.Background()
			h, driver, reader := newObservedBranchHarness(t)
			signer, err := agentsettings.NewGrantAuthority([]byte(strings.Repeat("g", 32)), h.now)
			if err != nil {
				t.Fatal(err)
			}
			issuer := &observedGrantEnvironment{AgentSettingsGrantEnvironment: agentsettings.NewGrantEnvironment(h.st, signer, h.now)}
			h.svc.SetAgentSettingsGrantEnvironment(issuer)
			first := completeTurn(t, h, "A", "provider-turn-1")
			h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool { return len(s.Messages) == 2 })
			oldGeneration := h.ctrl.Generation()
			edited, err := h.svc.EditMessage(ctx, testSession, first, ports.ChatUserMessage{Text: "A edited", ClientMessageID: "observed-edit", Origin: domain.MessageOriginHuman})
			if err != nil {
				t.Fatal(err)
			}
			if action != "edit" {
				current, err := h.svc.Controller(testSession)
				if err != nil {
					t.Fatal(err)
				}
				oldGeneration = current.Generation()
				driver.fresh.emit(ports.ChatEvent{Kind: ports.ChatEventTurnStarted, ProviderTurnID: "provider-turn-101"}, ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "provider-turn-101", TurnState: domain.TurnStateCompleted})
				h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
					for _, turn := range s.Turns {
						if turn.ID == edited.Turn.ID {
							return turn.State.Terminal()
						}
					}
					return false
				})
				if action == "restore" {
					restored := newFakeConversation()
					restored.providerConversationID = "thread-fresh"
					driver.fresh.mu.Lock()
					restored.settingsReceipt = driver.fresh.settingsReceipt
					driver.fresh.mu.Unlock()
					driver.mu.Lock()
					driver.resumed["thread-fresh"] = restored
					driver.beforeResume = func(cfg ports.ChatResumeConfig) error {
						if cfg.ProviderConversationID == "thread-1" {
							return errors.New("injected target failure")
						}
						return nil
					}
					driver.mu.Unlock()
				}
				_, err = h.svc.ActivateBranch(ctx, testSession, edited.SourceBranchID)
				if action == "restore" {
					if err == nil {
						t.Fatal("expected target failure with restored source")
					}
				} else if err != nil {
					t.Fatal(err)
				}
			}
			got, err := reader.Read(ctx, testSession)
			if err != nil {
				t.Fatal(err)
			}
			owner := got.Effective.Owner
			if got.Effective.FastMode.State != domain.AgentSettingNotProvided || owner == nil || owner.ControllerGeneration == oldGeneration || owner.ControllerGeneration != got.OwnerGeneration || owner.BranchID != got.BranchID || owner.ProviderConversationID != got.ProviderConversationID {
				t.Fatalf("%s replacement lost qualified receipt: %+v", action, got)
			}
			if action == "edit" && (got.CurrentTurn == nil || got.CurrentTurn.Revision == nil) {
				t.Fatalf("private edited dispatch lost revision: %+v", got.CurrentTurn)
			}
			issuer.mu.Lock()
			token := issuer.token
			issuer.mu.Unlock()
			id, scope, err := signer.Verify(token)
			if err != nil || id != testSession || scope.ActorGeneration != owner.ControllerGeneration {
				t.Fatalf("replacement received a stale settings grant: %+v %v", scope, err)
			}
		})
	}
}

func TestBranchReceiptAccountIsCapturedBeforeProviderStart(t *testing.T) {
	for _, action := range []string{"edit", "activate"} {
		t.Run(action, func(t *testing.T) {
			ctx := context.Background()
			h, driver, reader := newObservedBranchHarness(t)
			first := completeTurn(t, h, "A", "provider-turn-1")
			h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool { return len(s.Messages) == 2 })
			change := func() error {
				return h.st.BindSessionAgentAccount(ctx, testSession, domain.AgentAccountProviderCodex, "managed-replacement", time.Now())
			}
			if action == "edit" {
				driver.mu.Lock()
				driver.beforeStart = func(ports.ChatStartConfig) {
					if err := change(); err != nil {
						t.Error(err)
					}
				}
				driver.mu.Unlock()
			}
			edited, err := h.svc.EditMessage(ctx, testSession, first, ports.ChatUserMessage{Text: "A edited", ClientMessageID: "account-edit", Origin: domain.MessageOriginHuman})
			if err != nil {
				t.Fatal(err)
			}
			if action == "activate" {
				driver.fresh.emit(ports.ChatEvent{Kind: ports.ChatEventTurnStarted, ProviderTurnID: "provider-turn-101"}, ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "provider-turn-101", TurnState: domain.TurnStateCompleted})
				h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
					for _, turn := range s.Turns {
						if turn.ID == edited.Turn.ID {
							return turn.State.Terminal()
						}
					}
					return false
				})
				driver.mu.Lock()
				driver.beforeResume = func(ports.ChatResumeConfig) error { return change() }
				driver.mu.Unlock()
				if _, err := h.svc.ActivateBranch(ctx, testSession, edited.SourceBranchID); err != nil {
					t.Fatal(err)
				}
			}
			receipt, err := h.svc.ObserveAgentSettings(ctx, testSession)
			if err != nil {
				t.Fatal(err)
			}
			if receipt.Owner.AccountID != "device-codex" {
				t.Fatalf("late binding was borrowed as receipt identity: %+v", receipt.Owner)
			}
			got, err := reader.Read(ctx, testSession)
			if err != nil {
				t.Fatal(err)
			}
			if got.Effective.Model.State != domain.AgentSettingUnknown || got.Effective.Model.Reason != "observation_owner_mismatch" || got.CurrentTurn != nil {
				t.Fatalf("replacement borrowed new account evidence: %+v", got)
			}
		})
	}
}
