package chat_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/lifecycle"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

func successorBootstrapIntent(t *testing.T) (successorChatFixture, string) {
	t.Helper()
	return successorBootstrapIntentFor(t, successorChatReadyFixture(t))
}

func successorBootstrapIntentFor(t *testing.T, f successorChatFixture) (successorChatFixture, string) {
	t.Helper()
	prompt := "bounded handoff snapshot and immutable transcript path/hash"
	sum := sha256.Sum256([]byte(prompt))
	now := time.Now().UTC()
	f.op.Phase = domain.ReplacementControllerReady
	f.op.BootstrapClientID = "replacement:" + f.op.ID + ":bootstrap"
	f.op.Bootstrap = &domain.ReplacementBootstrap{PromptHash: hex.EncodeToString(sum[:]), IntentAt: now}
	if saved, err := f.store.SaveReplacementRuntimeProgress(context.Background(), f.op); err != nil || !saved {
		t.Fatal(saved, err)
	}
	var err error
	f.op, _, err = f.store.GetOrchestratorReplacement(context.Background(), f.op.ID)
	if err != nil {
		t.Fatal(err)
	}
	return f, prompt
}

func TestReplacementBootstrapRecoversExactReceiptWithoutResending(t *testing.T) {
	f, prompt := successorBootstrapIntent(t)
	ctx := context.Background()
	first, err := f.service.EnsureReplacementChatBootstrap(ctx, f.op, prompt, false)
	if err != nil || first.Uncertain || first.TurnID == "" || first.Owner != f.op.SuccessorController.Owner {
		t.Fatal("persisted intent with no canonical message could not recover", first, err)
	}
	again, err := f.service.EnsureReplacementChatBootstrap(ctx, f.op, prompt, false)
	if err != nil || again.TurnID != first.TurnID || again.Uncertain || len(f.provider.sentTexts()) != 1 {
		t.Fatal("bootstrap replayed", first, again, err)
	}
	if _, err := f.service.EnsureReplacementChatBootstrap(ctx, f.op, prompt+" changed", true); err == nil || len(f.provider.sentTexts()) != 1 {
		t.Fatal("changed bootstrap accepted")
	}
	f.op.Bootstrap.PromptHash = strings.Repeat("e", 64)
	if saved, err := f.store.SaveReplacementRuntimeProgress(ctx, f.op); saved || !errors.Is(err, domain.ErrReplacementStateInvalid) {
		t.Fatal("bootstrap intent was mutable", saved, err)
	}
}

func TestReplacementBootstrapCompletionRequiresMainNativeTurn(t *testing.T) {
	for _, provenance := range []string{"main", "child", "unknown"} {
		t.Run(provenance, func(t *testing.T) {
			f, prompt := successorBootstrapIntent(t)
			ctx := context.Background()
			receipt, err := f.service.EnsureReplacementChatBootstrap(ctx, f.op, prompt, true)
			if err != nil {
				t.Fatal(err)
			}
			f.provider.emit(ports.ChatEvent{Kind: ports.ChatEventTurnStarted, ProviderTurnID: "provider-turn-1"}, ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "provider-turn-1", TurnState: domain.TurnStateCompleted})
			deadline := time.Now().Add(5 * time.Second)
			for {
				turn, err := f.store.TurnByID(ctx, receipt.TurnID)
				if err != nil {
					t.Fatal(err)
				}
				if turn.State == domain.TurnStateCompleted {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("bootstrap did not settle", turn.State)
				}
				time.Sleep(time.Millisecond)
			}
			if provenance != "unknown" {
				native := f.op.SuccessorController.Owner.ProviderConversationID
				if provenance == "child" {
					native = "child-native"
				}
				conversation, err := f.store.ConversationForSession(ctx, f.target.ID)
				if err != nil {
					t.Fatal(err)
				}
				payload := fmt.Sprintf(`{"providerTurnId":%q,"providerConversationId":%q}`, "provider-turn-1", native)
				if _, err := f.store.ProjectProviderEvent(ctx, conversation.ID, f.target.ID, f.op.SuccessorController.Owner.ControllerGeneration, "bootstrap-identity", "turn.started", payload, time.Now(), func(context.Context) error { return nil }); err != nil {
					t.Fatal(err)
				}
			}
			observation, err := f.service.ObserveReplacementChatSuccessor(ctx, f.op)
			if provenance == "main" {
				if err != nil || observation.ControlTurn == nil || observation.ControlTurn.ID != receipt.TurnID || observation.ControlTurn.State != domain.TurnStateCompleted {
					t.Fatal("main bootstrap not verified", observation, err)
				}
			} else if err == nil && observation.ControlTurn != nil && observation.ControlTurn.State == domain.TurnStateCompleted {
				t.Fatal("unverified bootstrap passed", provenance, observation)
			}
		})
	}
}

func TestReplacementBootstrapReceiptCannotBeRewritten(t *testing.T) {
	for _, change := range []string{"intent", "clear", "turn", "settled", "clear settled"} {
		t.Run(change, func(t *testing.T) {
			f, _ := successorBootstrapIntent(t)
			ctx := context.Background()
			now := time.Now().UTC()
			f.op.Bootstrap.TurnID, f.op.Bootstrap.SettledAt = "bootstrap-turn", &now
			if saved, err := f.store.SaveReplacementRuntimeProgress(ctx, f.op); err != nil || !saved {
				t.Fatal(saved, err)
			}
			f.op, _, _ = f.store.GetOrchestratorReplacement(ctx, f.op.ID)
			switch change {
			case "intent":
				f.op.Bootstrap.IntentAt = f.op.Bootstrap.IntentAt.Add(time.Second)
			case "clear":
				f.op.Bootstrap = nil
			case "turn":
				f.op.Bootstrap.TurnID = "another-turn"
			case "settled":
				later := now.Add(time.Second)
				f.op.Bootstrap.SettledAt = &later
			case "clear settled":
				f.op.Bootstrap.SettledAt = nil
			}
			if saved, err := f.store.SaveReplacementRuntimeProgress(ctx, f.op); saved || !errors.Is(err, domain.ErrReplacementStateInvalid) {
				t.Fatal("durable bootstrap receipt rewritten", saved, err)
			}
		})
	}
}

type successorChatFixture struct {
	store    *sqlite.Store
	service  *chatsvc.Service
	provider *fakeConversation
	op       domain.OrchestratorReplacement
	target   domain.SessionRecord
}

// Source retirement is a durable precondition here; this fixture exercises the
// actual successor lifecycle callback and observation without a real provider.
func successorChatReadyFixture(t *testing.T) successorChatFixture {
	t.Helper()
	return successorChatReadyFixtureWithProvider(t, nil)
}

func successorChatReadyFixtureWithProvider(t *testing.T, wrap func(*fakeConversation) ports.ChatConversation) successorChatFixture {
	t.Helper()
	ctx := context.Background()
	store := openStore(t)
	source, _, err := store.GetSession(ctx, testSession)
	if err != nil {
		t.Fatal(err)
	}
	source.Harness = domain.HarnessClaudeCode
	source.Metadata.ControllerGeneration, source.Metadata.ProviderConversationID, source.Metadata.BrowserCapabilityVerifier = "source-generation", "source-native", "source-verifier"
	if err := store.UpdateSession(ctx, source); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	op, _, err := store.CreateOrchestratorReplacement(ctx, domain.OrchestratorReplacement{SchemaVersion: 1, ID: "11111111-1111-4111-8111-111111111111", OperationKey: "22222222-2222-4222-8222-222222222222", RequestHash: strings.Repeat("a", 64), ProjectID: testProject, SourceSessionID: source.ID, SourceOwner: source.ControllerOwner(), SourceCapabilityVerifier: source.Metadata.BrowserCapabilityVerifier, Phase: domain.ReplacementAccepted, CreatedAt: now, UpdatedAt: now, DeadlineAt: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	op.Phase, op.SourceRetiredAt = domain.ReplacementStarting, &now
	op.SpawnRequestID, op.SpawnRequestHash = "replacement:"+op.ID+":successor", strings.Repeat("b", 64)
	project, _, err := store.GetProject(ctx, string(testProject))
	if err != nil {
		t.Fatal(err)
	}
	op = sqlitetest.ReplacementPriorFacts(op)
	op = sqlitetest.SeedReplacement(t, project.Path, op)
	source.IsTerminated = true
	if err := store.UpdateSession(ctx, source); err != nil {
		t.Fatal(err)
	}
	target, _, err := store.CreateClientRequestSession(ctx, domain.SessionRecord{ProjectID: testProject, Kind: domain.KindOrchestrator, Harness: domain.HarnessClaudeCode, Mode: domain.SessionModeChat, ClientRequestID: op.SpawnRequestID, ClientRequestHash: op.SpawnRequestHash, ProvisionState: domain.SessionProvisionProvisioning, CreatedAt: now.Add(time.Second), UpdatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	if applied, err := store.UpdateBrowserCapabilityVerifier(ctx, target.ID, target.ControllerOwner(), "target-verifier"); err != nil || !applied {
		t.Fatal(applied, err)
	}
	provider := newFakeConversation()
	var native ports.ChatConversation = provider
	if wrap != nil {
		native = wrap(provider)
	}
	var ids atomic.Int64
	service := chatsvc.New(chatsvc.Options{Store: store, Reader: fullSnapshotReader(store), Sessions: store, Drivers: fakeRegistry{driver: fakeDriver{conv: native}}, Log: slog.New(slog.DiscardHandler), NewID: func() string { return fmt.Sprintf("successor-%d", ids.Add(1)) }})
	t.Cleanup(func() { service.StopAll(context.Background()) })
	manager := lifecycle.New(store, nil)
	_, err = service.Start(ctx, chatsvc.StartConfig{SessionID: target.ID, ProjectID: testProject, Kind: domain.KindOrchestrator, Harness: domain.HarnessClaudeCode, WorkspacePath: t.TempDir(), ControllerReady: func(started ports.ChatControllerStarted) (ports.ChatControllerCommit, error) {
		metadata := domain.SessionMetadata{ControllerGeneration: started.ControllerGeneration, ProviderConversationID: started.ProviderConversationID, BrowserCapabilityVerifier: "target-verifier"}
		var err error
		if started.ProviderBoundary == nil {
			err = manager.MarkSpawned(ctx, target.ID, metadata)
		} else if started.CommitProviderHistory == nil {
			err = manager.MarkChatSpawned(ctx, target.ID, metadata, *started.ProviderBoundary)
		} else {
			err = manager.MarkChatSpawnedPrepared(ctx, target.ID, metadata, *started.ProviderBoundary, nil, started.CommitProviderHistory)
		}
		conversation, readErr := store.ConversationForSession(ctx, target.ID)
		if err == nil {
			err = readErr
		}
		return ports.ChatControllerCommit{Conversation: conversation}, err
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetSessionProvisionState(ctx, target.ID, domain.SessionProvisionReady, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	target, _, err = store.GetSession(ctx, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	op, _, err = store.GetOrchestratorReplacement(ctx, op.ID)
	if err != nil || op.SuccessorController == nil {
		t.Fatal("actual lifecycle callback did not bind receipt", err)
	}
	return successorChatFixture{store: store, service: service, provider: provider, op: op, target: target}
}

func TestReplacementSuccessorObservationRequiresCommittedLiveOwner(t *testing.T) {
	for _, change := range []string{"ready", "provisioning", "stale revision", "wrong key", "generation", "native", "capability", "missing controller", "active branch"} {
		t.Run(change, func(t *testing.T) {
			f := successorChatReadyFixture(t)
			ctx := context.Background()
			op, record := f.op, f.target
			switch change {
			case "provisioning":
				if _, err := f.store.SetSessionProvisionState(ctx, record.ID, domain.SessionProvisionProvisioning, "", time.Now()); err != nil {
					t.Fatal(err)
				}
			case "stale revision":
				op.Revision--
			case "wrong key":
				op.SpawnRequestHash = strings.Repeat("e", 64)
			case "generation":
				record.Metadata.ControllerGeneration = "different-generation"
			case "native":
				record.Metadata.ProviderConversationID = "different-native"
			case "capability":
				if applied, err := f.store.UpdateBrowserCapabilityVerifier(ctx, record.ID, record.ControllerOwner(), "different-verifier"); err != nil || !applied {
					t.Fatal(applied, err)
				}
			case "missing controller":
				if err := f.service.Stop(ctx, record.ID); err != nil {
					t.Fatal(err)
				}
			case "active branch":
				conversation, err := f.store.ConversationForSession(ctx, record.ID)
				if err != nil {
					t.Fatal(err)
				}
				err = f.store.CreateConversationBranch(ctx, domain.ConversationBranch{ID: "different-branch", ConversationID: conversation.ID, SessionID: record.ID, ProviderConversationID: record.Metadata.ProviderConversationID, CreatedAt: time.Now()}, time.Now())
				if err != nil {
					t.Fatal(err)
				}
				if err := f.store.ActivateConversationBranch(ctx, record.ID, conversation.ID, "different-branch", record.Metadata.ProviderConversationID, record.Metadata.ControllerGeneration, time.Now()); err != nil {
					t.Fatal(err)
				}
			}
			if change == "generation" || change == "native" {
				if err := f.store.UpdateSession(ctx, record); err != nil {
					t.Fatal(err)
				}
			}
			before, _, err := f.store.GetSession(ctx, record.ID)
			if err != nil {
				t.Fatal(err)
			}
			observation, err := f.service.ObserveReplacementChatSuccessor(ctx, op)
			if change == "ready" {
				if err != nil || observation.State != ports.ChatControllerReady || observation.Owner != f.op.SuccessorController.Owner || observation.NativeID != f.target.Metadata.ProviderConversationID || observation.Provenance != domain.NativeThreadMain || observation.ObservedAt.IsZero() {
					t.Fatalf("ready owner not positively observed: %+v %v", observation, err)
				}
			} else if change == "provisioning" {
				if err != nil || observation.State == ports.ChatControllerReady {
					t.Fatal("provisioning seed claimed ready", err)
				}
			} else if err == nil {
				t.Fatal("unverified owner claimed ready", change, observation)
			}
			after, _, readErr := f.store.GetSession(ctx, record.ID)
			if readErr != nil || !reflect.DeepEqual(before, after) || len(f.provider.sentTexts()) != 0 {
				t.Fatal("observation mutated source or delivered a prompt", readErr)
			}
			if change == "missing controller" && f.service.HasLiveChatController(record.ID) {
				t.Fatal("observation woke a provider")
			}
		})
	}
}
