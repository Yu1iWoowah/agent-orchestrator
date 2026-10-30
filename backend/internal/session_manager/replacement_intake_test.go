package sessionmanager

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type replacementDrainStore struct {
	*replacementFenceStore
	fenced bool
}

func (s *replacementDrainStore) ReplacementWakeFenced(context.Context, domain.SessionID) (bool, error) {
	return s.fenced, nil
}

type intakeObserverChat struct {
	*recordingLauncher
	observed ports.ReplacementSourceObservation
	drained  []domain.SessionID
}

func (c *intakeObserverChat) ObserveReplacementChatSuccessor(context.Context, domain.OrchestratorReplacement) (ports.ReplacementSourceObservation, error) {
	return c.observed, nil
}
func (c *intakeObserverChat) DrainChatQueue(_ context.Context, id domain.SessionID) error {
	c.drained = append(c.drained, id)
	return nil
}

func TestReplacementIntakeDrainOnlyKicksReleasedResumedOwner(t *testing.T) {
	for _, fault := range []string{"current", "busy", "revision", "generation", "capability", "unreleased", "fenced", "wrong phase", "missing writer", "child", "wrong native", "unknown state", "no timestamp", "missing observer"} {
		t.Run(fault, func(t *testing.T) {
			m, base, rt, workspace := newManager()
			now := time.Now().UTC()
			target := mkLive("mer-successor")
			target.Kind, target.Harness, target.Mode = domain.KindOrchestrator, domain.HarnessClaudeCode, domain.SessionModeChat
			target.ClientRequestID, target.ClientRequestHash = "target-key", "target-hash"
			target.ProvisionState = domain.SessionProvisionReady
			target.Metadata.ControllerGeneration, target.Metadata.ProviderConversationID, target.Metadata.BrowserCapabilityVerifier = "after", "target-native", "after-verifier"
			before := target.ControllerOwner()
			before.ControllerGeneration = "before"
			base.sessions[target.ID] = target
			op := domain.OrchestratorReplacement{ID: "replacement", ProjectID: target.ProjectID, SourceSessionID: "old-source", SourceRetiredAt: &now, Phase: domain.ReplacementNotifying, Revision: 7, SuccessorSessionID: target.ID, SpawnRequestID: target.ClientRequestID, SpawnRequestHash: target.ClientRequestHash,
				SuccessorController: &domain.ReplacementSuccessorController{Owner: before, CapabilityVerifier: "before-verifier", CommittedAt: now},
				ResumeEvidence:      &domain.ReplacementResumeEvidence{StartedAt: now, BeforeOwner: before, BeforeNativeID: before.ProviderConversationID, BeforeCapabilityVerifier: "before-verifier", StopConfirmed: true, StopReceipt: &domain.ReplacementControllerStop{Owner: before, NativeID: before.ProviderConversationID, CapabilityVerifier: "before-verifier", Method: "chat-hibernate", ConfirmedAt: now}, ResumeIntentAt: &now, ResumeGeneration: "after", ResumedAt: &now, AfterOwner: target.ControllerOwner(), AfterNativeID: before.ProviderConversationID, AfterCapabilityVerifier: "after-verifier", HistoryVerified: true, VerifiedAt: &now},
				WriterActivation:    &domain.ReplacementWriterActivation{BeforeEpoch: 1, AfterEpoch: 2, Owner: target.ControllerOwner(), CapabilityVerifier: "after-verifier", ActivatedAt: now}, IntakeReleasedAt: &now,
			}
			st := &replacementDrainStore{replacementFenceStore: &replacementFenceStore{fakeStore: base, operation: op}}
			chat := &intakeObserverChat{recordingLauncher: &recordingLauncher{}, observed: ports.ReplacementSourceObservation{Owner: target.ControllerOwner(), NativeID: target.Metadata.ProviderConversationID, Provenance: domain.NativeThreadMain, State: ports.ChatControllerReady, ObservedAt: now}}
			m.store, m.chat = st, chat
			expected := op
			switch fault {
			case "busy":
				chat.observed.State = ports.ChatControllerBusy
			case "revision":
				expected.Revision--
			case "generation":
				target.Metadata.ControllerGeneration = "rotated"
				base.sessions[target.ID] = target
			case "capability":
				target.Metadata.BrowserCapabilityVerifier = "rotated"
				base.sessions[target.ID] = target
			case "unreleased":
				expected.IntakeReleasedAt = nil
			case "fenced":
				st.fenced = true
			case "wrong phase":
				expected.Phase = domain.ReplacementActivatingSharedWriter
			case "missing writer":
				expected.WriterActivation = nil
			case "child":
				chat.observed.Provenance = domain.NativeThreadChild
			case "wrong native":
				chat.observed.NativeID = "unverified-native"
			case "unknown state":
				chat.observed.State = ""
			case "no timestamp":
				chat.observed.ObservedAt = time.Time{}
			case "missing observer":
				m.chat = &recordingLauncher{}
			}
			api, ok := any(m).(interface {
				DrainReplacementIntake(context.Context, domain.OrchestratorReplacement) error
			})
			if !ok {
				t.Fatal("verified queue-kick boundary unavailable")
			}
			err := api.DrainReplacementIntake(context.Background(), expected)
			if fault == "current" || fault == "busy" {
				if err != nil || len(chat.drained) != 1 || chat.drained[0] != target.ID {
					t.Fatal("verified existing queue not kicked", chat.drained, err)
				}
			} else if err == nil || len(chat.drained) != 0 {
				t.Fatal("unverified queue kick escaped", fault, chat.drained, err)
			}
			if len(chat.started) != 0 || len(chat.stopped) != 0 || len(chat.background) != 0 || rt.created != 0 || rt.destroyed != 0 || workspace.destroyed != 0 {
				t.Fatal("queue kick performed lifecycle action")
			}
		})
	}
}

func TestReplacementIntakeDrainRejectsUnverifiedOperationBeforeObserver(t *testing.T) {
	m, base, _, _ := newManager()
	m.store = &replacementDrainStore{replacementFenceStore: &replacementFenceStore{fakeStore: base}}
	api, ok := any(m).(interface {
		DrainReplacementIntake(context.Context, domain.OrchestratorReplacement) error
	})
	if !ok {
		t.Fatal("verified queue-kick boundary unavailable")
	}
	err := api.DrainReplacementIntake(context.Background(), domain.OrchestratorReplacement{})
	if !errors.Is(err, domain.ErrReplacementStateInvalid) {
		t.Fatal("unverified operation was accepted", err)
	}
}
