package sessionmanager

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type replacementRuntimeChat struct {
	*recordingLauncher
	observed, prepared, stopped int
	allowNew                    bool
}

func (c *replacementRuntimeChat) ObserveReplacementChatSuccessor(_ context.Context, op domain.OrchestratorReplacement) (ports.ReplacementSourceObservation, error) {
	c.observed++
	proof, ok := op.ExpectedSuccessorController()
	if !ok {
		return ports.ReplacementSourceObservation{}, domain.ErrReplacementStateInvalid
	}
	return ports.ReplacementSourceObservation{Owner: proof.Owner, NativeID: proof.Owner.ProviderConversationID, Provenance: domain.NativeThreadMain, State: ports.ChatControllerReady, ObservedAt: time.Now()}, nil
}

func (c *replacementRuntimeChat) EnsureReplacementChatProbe(_ context.Context, op domain.OrchestratorReplacement, _ string, allowNew bool) (ports.ReplacementPreparationDelivery, error) {
	c.prepared++
	c.allowNew = allowNew
	return ports.ReplacementPreparationDelivery{Owner: op.ResumeEvidence.AfterOwner, NativeID: op.ResumeEvidence.AfterNativeID, TurnID: "probe-turn"}, nil
}

func TestReplacementProbeManagerFencesInitialAndStaleOwners(t *testing.T) {
	for _, change := range []string{"resumed owner", "stale revision", "old owner", "old verifier", "missing probe", "before resume"} {
		t.Run(change, func(t *testing.T) {
			m, store, chat := replacementRuntimeManager(t)
			target := store.sessions[store.operation.SourceSessionID]
			target.ID, target.ClientRequestID, target.ClientRequestHash = "successor", "target-key", "target-hash"
			target.Metadata.ControllerGeneration, target.Metadata.ProviderConversationID, target.Metadata.BrowserCapabilityVerifier = "before", "target-native", "before-verifier"
			target.ProvisionState = domain.SessionProvisionReady
			before := target.ControllerOwner()
			target.Metadata.ControllerGeneration, target.Metadata.BrowserCapabilityVerifier = "after", "after-verifier"
			store.sessions[target.ID] = target
			now := time.Now().UTC()
			op := &store.operation
			op.Phase, op.SourceRetiredAt = domain.ReplacementVerifyingResume, &now
			op.SuccessorSessionID, op.SpawnRequestID, op.SpawnRequestHash = target.ID, target.ClientRequestID, target.ClientRequestHash
			op.SuccessorController = &domain.ReplacementSuccessorController{Owner: before, CapabilityVerifier: "before-verifier", CommittedAt: now}
			op.ResumeEvidence = &domain.ReplacementResumeEvidence{StartedAt: now, BeforeOwner: before, BeforeNativeID: "target-native", BeforeCapabilityVerifier: "before-verifier", StopConfirmed: true, StopReceipt: &domain.ReplacementControllerStop{Owner: before, NativeID: "target-native", CapabilityVerifier: "before-verifier", Method: "chat-hibernate", ConfirmedAt: now}, ResumeIntentAt: &now, ResumeGeneration: "after", ResumedAt: &now, AfterOwner: target.ControllerOwner(), AfterNativeID: "target-native", AfterCapabilityVerifier: "after-verifier", HistoryVerified: true, Probe: &domain.ReplacementContinuityProbe{Nonce: "44444444-4444-4444-8444-444444444444", PromptHash: strings.Repeat("a", 64), IntentAt: now}}
			expected := *op
			switch change {
			case "stale revision":
				expected.Revision--
			case "old owner":
				target.Metadata.ControllerGeneration = "before"
				store.sessions[target.ID] = target
			case "old verifier":
				target.Metadata.BrowserCapabilityVerifier = "before-verifier"
				store.sessions[target.ID] = target
			case "missing probe":
				op.ResumeEvidence.Probe = nil
			case "before resume":
				op.ResumeEvidence.ResumedAt = nil
			}
			receipt, err := m.EnsureReplacementProbe(context.Background(), expected, "exact stored probe", true)
			if change == "resumed owner" {
				if err != nil || receipt.Owner != target.ControllerOwner() || chat.prepared != 1 || !chat.allowNew {
					t.Fatal(receipt, err)
				}
			} else if err == nil || chat.prepared != 0 {
				t.Fatal("stale owner sent probe", change, err)
			}
			if chat.stopped != 0 || len(chat.started) != 0 {
				t.Fatal("probe changed lifecycle")
			}
		})
	}
}

func (c *replacementRuntimeChat) QuiesceReplacementChatSuccessor(_ context.Context, op domain.OrchestratorReplacement) (domain.ReplacementControllerStop, error) {
	c.stopped++
	return domain.ReplacementControllerStop{Owner: op.ResumeEvidence.BeforeOwner, NativeID: op.ResumeEvidence.BeforeNativeID, CapabilityVerifier: op.ResumeEvidence.BeforeCapabilityVerifier, Method: "chat-hibernate", ConfirmedAt: time.Now()}, nil
}

func TestReplacementSuccessorStopFencesManagerReservation(t *testing.T) {
	for _, change := range []string{"bound", "stale revision", "missing intent", "wrong before owner", "rotated capability"} {
		t.Run(change, func(t *testing.T) {
			m, store, chat := replacementRuntimeManager(t)
			target := store.sessions[store.operation.SourceSessionID]
			target.ID, target.ClientRequestID, target.ClientRequestHash = "successor", "target-key", "target-hash"
			target.Metadata.ControllerGeneration, target.Metadata.ProviderConversationID = "target-generation", "target-native"
			target.ProvisionState = domain.SessionProvisionReady
			store.sessions[target.ID] = target
			now := time.Now().UTC()
			op := &store.operation
			op.Phase, op.SourceRetiredAt, op.FrozenAt = domain.ReplacementVerifyingResume, &now, &now
			op.Transcript = &domain.ReplacementTranscript{}
			op.SuccessorSessionID, op.SpawnRequestID, op.SpawnRequestHash = target.ID, target.ClientRequestID, target.ClientRequestHash
			op.SuccessorController = &domain.ReplacementSuccessorController{Owner: target.ControllerOwner(), CapabilityVerifier: target.Metadata.BrowserCapabilityVerifier, CommittedAt: now}
			op.Bootstrap = &domain.ReplacementBootstrap{TurnID: "bootstrap", SettledAt: &now}
			op.ResumeEvidence = &domain.ReplacementResumeEvidence{StartedAt: now, BeforeOwner: target.ControllerOwner(), BeforeNativeID: target.Metadata.ProviderConversationID, BeforeCapabilityVerifier: target.Metadata.BrowserCapabilityVerifier}
			expected := *op
			switch change {
			case "stale revision":
				expected.Revision--
			case "missing intent":
				op.ResumeEvidence = nil
			case "wrong before owner":
				op.ResumeEvidence.BeforeOwner.ControllerGeneration = "different"
			case "rotated capability":
				target.Metadata.BrowserCapabilityVerifier = "different"
				store.sessions[target.ID] = target
			}
			stop, err := m.QuiesceReplacementSuccessor(context.Background(), expected)
			if change == "bound" {
				if err != nil || stop.Owner != target.ControllerOwner() || chat.stopped != 1 {
					t.Fatal(stop, chat.stopped, err)
				}
			} else if err == nil || chat.stopped != 0 || !stop.ConfirmedAt.IsZero() {
				t.Fatal("stale target stopped", change, chat.stopped, err)
			}
		})
	}
}

func TestReplacementSuccessorObservationFencesManagerReservation(t *testing.T) {
	for _, change := range []string{"ready", "stale revision", "wrong target", "wrong phase", "not retired", "rotated owner"} {
		t.Run(change, func(t *testing.T) {
			m, store, chat := replacementRuntimeManager(t)
			target := store.sessions[store.operation.SourceSessionID]
			target.ID, target.ClientRequestID, target.ClientRequestHash = "successor", "successor-key", "successor-hash"
			target.Metadata.ControllerGeneration, target.Metadata.ProviderConversationID = "target-generation", "target-native"
			target.ProvisionState = domain.SessionProvisionReady
			store.sessions[target.ID] = target
			now := time.Now()
			store.operation.Phase, store.operation.SourceRetiredAt, store.operation.SuccessorSessionID = domain.ReplacementStarting, &now, target.ID
			store.operation.SpawnRequestID, store.operation.SpawnRequestHash = target.ClientRequestID, target.ClientRequestHash
			store.operation.SuccessorController = &domain.ReplacementSuccessorController{Owner: target.ControllerOwner(), CapabilityVerifier: target.Metadata.BrowserCapabilityVerifier, CommittedAt: now}
			expected := store.operation
			switch change {
			case "stale revision":
				expected.Revision--
			case "wrong target":
				expected.SuccessorSessionID = "another"
			case "wrong phase":
				store.operation.Phase = domain.ReplacementNotifying
			case "not retired":
				store.operation.SourceRetiredAt = nil
			case "rotated owner":
				target.Metadata.ControllerGeneration = "rotated"
				store.sessions[target.ID] = target
			}
			observation, err := m.ObserveReplacementSuccessor(context.Background(), expected)
			if change == "ready" {
				if err != nil || observation.Owner != target.ControllerOwner() || chat.observed != 1 {
					t.Fatal(observation, chat.observed, err)
				}
			} else if err == nil || chat.observed != 0 {
				t.Fatal("stale reservation delegated to provider", chat.observed, err)
			}
			if chat.stopped != 0 || chat.prepared != 0 {
				t.Fatal("observation caused provider action")
			}
		})
	}
}

func (c *replacementRuntimeChat) ObserveReplacementChatSource(_ context.Context, op domain.OrchestratorReplacement) (ports.ReplacementSourceObservation, error) {
	c.observed++
	return ports.ReplacementSourceObservation{Owner: op.SourceOwner, NativeID: op.SourceOwner.ProviderConversationID, Provenance: domain.NativeThreadMain, State: ports.ChatControllerReady, ObservedAt: time.Now()}, nil
}
func (c *replacementRuntimeChat) EnsureReplacementChatPreparation(_ context.Context, op domain.OrchestratorReplacement, _ string, allowNew bool) (ports.ReplacementPreparationDelivery, error) {
	c.prepared++
	c.allowNew = allowNew
	return ports.ReplacementPreparationDelivery{TurnID: "source-control-turn", Owner: op.SourceOwner, NativeID: op.SourceOwner.ProviderConversationID}, nil
}
func (c *replacementRuntimeChat) QuiesceReplacementChatSource(_ context.Context, op domain.OrchestratorReplacement) (domain.ReplacementControllerStop, error) {
	c.stopped++
	return domain.ReplacementControllerStop{Owner: op.SourceOwner, CapabilityVerifier: op.SourceCapabilityVerifier, NativeID: op.SourceOwner.ProviderConversationID, Method: "chat-hibernate", ConfirmedAt: time.Now()}, nil
}

func replacementRuntimeManager(t *testing.T) (*Manager, *replacementFenceStore, *replacementRuntimeChat) {
	t.Helper()
	chat := &replacementRuntimeChat{recordingLauncher: &recordingLauncher{}}
	m, store, _ := newChatManager(chat)
	source := mkLive("mer-1")
	source.Kind, source.Harness, source.Mode = domain.KindOrchestrator, domain.HarnessClaudeCode, domain.SessionModeChat
	source.Metadata.ControllerGeneration, source.Metadata.ProviderConversationID, source.Metadata.BrowserCapabilityVerifier = "generation", "native-source", "source-verifier"
	store.sessions[source.ID] = source
	fenced := &replacementFenceStore{fakeStore: store, operation: domain.OrchestratorReplacement{ID: "operation", ProjectID: source.ProjectID, SourceSessionID: source.ID, SourceOwner: source.ControllerOwner(), SourceCapabilityVerifier: source.Metadata.BrowserCapabilityVerifier, Phase: domain.ReplacementPreparingHandoff, Revision: 4}}
	m.store = fenced
	return m, fenced, chat
}

func TestReplacementRuntimeChatUsesBoundProjectReservation(t *testing.T) {
	m, store, chat := replacementRuntimeManager(t)
	op := store.operation
	if err := m.WithOrchestratorProjectGate(context.Background(), op.ProjectID, func(locked context.Context) error {
		observation, err := m.ObserveReplacementSource(locked, op)
		if err != nil || observation.Owner != op.SourceOwner {
			t.Fatalf("observation=%+v err=%v", observation, err)
		}
		delivery, err := m.EnsureReplacementPreparation(locked, op, "preparation", false)
		if err != nil || delivery.TurnID != "source-control-turn" || chat.allowNew {
			t.Fatalf("delivery=%+v err=%v", delivery, err)
		}
		store.operation.Phase = domain.ReplacementExportingTranscript
		op = store.operation
		stop, err := m.QuiesceReplacementSource(locked, op)
		if err != nil || stop.Owner != op.SourceOwner {
			t.Fatalf("stop=%+v err=%v", stop, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if chat.observed != 1 || chat.prepared != 1 || chat.stopped != 1 {
		t.Fatal("bound runtime calls were not delegated once")
	}
}

func TestReplacementRuntimeRefusesStaleBindingBeforeProviderEffects(t *testing.T) {
	for _, changed := range []string{"revision", "operation", "capability", "owner", "worker", "wrong project"} {
		t.Run(changed, func(t *testing.T) {
			m, store, chat := replacementRuntimeManager(t)
			op := store.operation
			rec := store.sessions[op.SourceSessionID]
			switch changed {
			case "revision":
				op.Revision--
			case "operation":
				op.ID = "other-operation"
			case "capability":
				rec.Metadata.BrowserCapabilityVerifier = "rotated"
			case "owner":
				rec.Metadata.ControllerGeneration = "rotated"
			case "worker":
				rec.Kind = domain.KindWorker
			case "wrong project":
				rec.ProjectID = "another-project"
			}
			store.sessions[rec.ID] = rec
			if _, err := m.ObserveReplacementSource(context.Background(), op); !errors.Is(err, domain.ErrReplacementSourceChanged) {
				t.Fatalf("observe stale binding: %v", err)
			}
			if _, err := m.EnsureReplacementPreparation(context.Background(), op, "preparation", true); !errors.Is(err, domain.ErrReplacementSourceChanged) {
				t.Fatalf("prepare stale binding: %v", err)
			}
			if _, err := m.QuiesceReplacementSource(context.Background(), op); !errors.Is(err, domain.ErrReplacementSourceChanged) {
				t.Fatalf("stop stale binding: %v", err)
			}
			if chat.observed != 0 || chat.prepared != 0 || chat.stopped != 0 {
				t.Fatal("stale operation reached provider")
			}
		})
	}
}
