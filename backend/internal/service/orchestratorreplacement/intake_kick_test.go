package orchestratorreplacement_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	replacement "github.com/aoagents/agent-orchestrator/backend/internal/service/orchestratorreplacement"
)

// This coordinator port supplies already-verified facts. SQL authority, CAS,
// lease/receipt integrity and rollback are tested against the real store.
type intakeCoordinatorStore struct {
	replacement.Store
	op                            domain.OrchestratorReplacement
	target                        domain.SessionRecord
	project                       domain.ProjectRecord
	fenced, lostRelease, lostKick bool
	releases, kicks               int
}

func (s *intakeCoordinatorStore) GetOrchestratorReplacement(_ context.Context, id string) (domain.OrchestratorReplacement, bool, error) {
	return s.op, id == s.op.ID, nil
}
func (s *intakeCoordinatorStore) GetSession(_ context.Context, id domain.SessionID) (domain.SessionRecord, bool, error) {
	return s.target, id == s.target.ID, nil
}
func (s *intakeCoordinatorStore) GetProject(_ context.Context, id string) (domain.ProjectRecord, bool, error) {
	return s.project, id == s.project.ID, nil
}
func (s *intakeCoordinatorStore) ReplacementWakeFenced(context.Context, domain.SessionID) (bool, error) {
	proof, ok := s.op.ExpectedSuccessorController()
	return s.fenced || !ok || s.target.ControllerOwner() != proof.Owner || s.target.Metadata.BrowserCapabilityVerifier != proof.CapabilityVerifier, nil
}
func (s *intakeCoordinatorStore) SaveOrchestratorReplacement(_ context.Context, op domain.OrchestratorReplacement) (bool, error) {
	if op.Revision != s.op.Revision {
		return false, nil
	}
	op.Revision++
	s.op = op
	return true, nil
}
func (s *intakeCoordinatorStore) ReleaseReplacementIntake(_ context.Context, id string, revision int64, at time.Time) (domain.OrchestratorReplacement, bool, error) {
	s.releases++
	if id != s.op.ID || revision != s.op.Revision {
		return s.op, false, nil
	}
	s.op.IntakeReleasedAt = &at
	s.op.Revision++
	if s.lostRelease {
		s.lostRelease = false
		return domain.OrchestratorReplacement{}, false, errors.New("release reply lost after commit")
	}
	return s.op, true, nil
}
func (s *intakeCoordinatorStore) RecordReplacementIntakeDrain(_ context.Context, id string, revision int64, at time.Time) (domain.OrchestratorReplacement, bool, error) {
	s.kicks++
	proof, valid := s.op.ExpectedSuccessorController()
	if id != s.op.ID || revision != s.op.Revision || s.fenced || !valid || s.target.ControllerOwner() != proof.Owner || s.target.Metadata.BrowserCapabilityVerifier != proof.CapabilityVerifier {
		return s.op, false, nil
	}
	s.op.IntakeDrainRequestedAt = &at
	s.op.Revision++
	if s.lostKick {
		s.lostKick = false
		return domain.OrchestratorReplacement{}, false, errors.New("kick reply lost after commit")
	}
	return s.op, true, nil
}

type intakeCoordinatorRuntime struct {
	*runtime
	store                         *intakeCoordinatorStore
	drains, successorObservations int
	onDrain                       func()
}

func (r *intakeCoordinatorRuntime) ObserveReplacementSuccessor(context.Context, domain.OrchestratorReplacement) (ports.ReplacementSourceObservation, error) {
	r.successorObservations++
	return ports.ReplacementSourceObservation{Owner: r.store.target.ControllerOwner(), NativeID: r.store.target.Metadata.ProviderConversationID, Provenance: domain.NativeThreadMain, State: ports.ChatControllerReady, ObservedAt: time.Now().UTC()}, nil
}
func (r *intakeCoordinatorRuntime) DrainReplacementIntake(context.Context, domain.OrchestratorReplacement) error {
	r.drains++
	if r.onDrain != nil {
		r.onDrain()
	}
	return nil
}
func intakeKickFixture(t *testing.T) (*replacement.Service, *intakeCoordinatorStore, *intakeCoordinatorRuntime) {
	t.Helper()
	now := time.Now().UTC()
	target := domain.SessionRecord{ID: "successor", ProjectID: "project", Kind: domain.KindOrchestrator, Harness: domain.HarnessClaudeCode, Mode: domain.SessionModeChat, ProvisionState: domain.SessionProvisionReady, Metadata: domain.SessionMetadata{ControllerGeneration: "after", ProviderConversationID: "target-native", BrowserCapabilityVerifier: "after-verifier"}}
	before := target.ControllerOwner()
	before.ControllerGeneration = "before"
	op := domain.OrchestratorReplacement{SchemaVersion: 1, ID: "11111111-1111-4111-8111-111111111111", ProjectID: "project", SourceSessionID: "source", SourceRetiredAt: &now, Phase: domain.ReplacementNotifying, Revision: 7, SuccessorSessionID: target.ID,
		SuccessorController: &domain.ReplacementSuccessorController{Owner: before, CapabilityVerifier: "before-verifier", CommittedAt: now},
		ResumeEvidence:      &domain.ReplacementResumeEvidence{StartedAt: now, BeforeOwner: before, BeforeNativeID: before.ProviderConversationID, BeforeCapabilityVerifier: "before-verifier", StopConfirmed: true, StopReceipt: &domain.ReplacementControllerStop{Owner: before, NativeID: before.ProviderConversationID, CapabilityVerifier: "before-verifier", Method: "chat-hibernate", ConfirmedAt: now}, ResumeIntentAt: &now, ResumeGeneration: "after", ResumedAt: &now, AfterOwner: target.ControllerOwner(), AfterNativeID: before.ProviderConversationID, AfterCapabilityVerifier: "after-verifier", HistoryVerified: true, VerifiedAt: &now},
		WriterActivation:    &domain.ReplacementWriterActivation{BeforeEpoch: 1, AfterEpoch: 2, Owner: target.ControllerOwner(), CapabilityVerifier: "after-verifier", ActivatedAt: now},
		Notifications:       []domain.ReplacementWorkerNotification{{SessionID: "worker", ContactID: "handoff-notice:11111111-1111-4111-8111-111111111111:worker", State: "pending", CreatedAt: now}},
	}
	project := domain.ProjectRecord{ID: "project", Path: t.TempDir()}
	var err error
	op.ProjectFingerprint, err = domain.ReplacementProjectFingerprint(project)
	if err != nil {
		t.Fatal(err)
	}
	st := &intakeCoordinatorStore{op: op, target: target, project: project}
	rt := &intakeCoordinatorRuntime{runtime: &runtime{}, store: st}
	return replacement.New(replacement.Deps{Store: st, Runtime: rt}), st, rt
}

func TestReplacementReleaseCrashBeforeKickIsRecoverable(t *testing.T) {
	svc, st, rt := intakeKickFixture(t)
	now := time.Now().UTC()
	st.op.IntakeReleasedAt = &now
	result, err := svc.Advance(context.Background(), st.op.ID)
	if err != nil || result.Phase != domain.ReplacementNotifying || result.IntakeDrainRequestedAt == nil || rt.drains != 1 || st.releases != 0 {
		t.Fatal("crash gap lost durable kick", result, rt.drains, err)
	}
	again, err := replacement.New(replacement.Deps{Store: st, Runtime: rt}).Advance(context.Background(), st.op.ID)
	if err != nil || again.Revision != result.Revision || rt.drains != 1 || len(again.Notifications) != 1 {
		t.Fatal("restart repeated kick or completed notification", again, rt.drains, err)
	}
}
func TestReplacementKickReceiptSurvivesReplyLossWithoutRepeatingProvider(t *testing.T) {
	for _, which := range []string{"release", "kick"} {
		t.Run(which, func(t *testing.T) {
			svc, st, rt := intakeKickFixture(t)
			st.lostRelease = which == "release"
			st.lostKick = which == "kick"
			result, err := svc.Advance(context.Background(), st.op.ID)
			if err != nil || result.IntakeReleasedAt == nil || result.IntakeDrainRequestedAt == nil || rt.drains != 1 || st.releases != 1 || st.kicks != 1 {
				t.Fatal("committed receipt discarded", result, rt.drains, st.releases, st.kicks, err)
			}
			again, err := svc.Advance(context.Background(), st.op.ID)
			if err != nil || again.Revision != result.Revision || rt.drains != 1 || st.releases != 1 || st.kicks != 1 || again.Phase != domain.ReplacementNotifying {
				t.Fatal("receipt replay repeated provider or completed operation", again, rt.drains, err)
			}
		})
	}
}
func TestReplacementCompletedKickChecksCurrentWriterWithoutProvider(t *testing.T) {
	for _, change := range []string{"generation", "capability", "lease"} {
		t.Run(change, func(t *testing.T) {
			svc, st, rt := intakeKickFixture(t)
			first, err := svc.Advance(context.Background(), st.op.ID)
			if err != nil || first.IntakeDrainRequestedAt == nil {
				t.Fatal(first, err)
			}
			switch change {
			case "generation":
				st.target.Metadata.ControllerGeneration = "rotated"
			case "capability":
				st.target.Metadata.BrowserCapabilityVerifier = "rotated"
			case "lease":
				st.fenced = true
			}
			next, err := svc.Advance(context.Background(), st.op.ID)
			if err != nil || next.Phase != domain.ReplacementRecoveryRequired || next.Error == nil || next.Error.Code != "ORCHESTRATOR_HANDOFF_SUCCESSOR_OWNER_CHANGED" || rt.drains != 1 {
				t.Fatal("stale writer repeated kick", next, rt.drains, err)
			}
		})
	}
}
func TestReplacementKickReceiptRejectsRotationAfterRuntimeReturns(t *testing.T) {
	svc, st, rt := intakeKickFixture(t)
	rt.onDrain = func() { st.target.Metadata.BrowserCapabilityVerifier = "rotated" }
	result, err := svc.Advance(context.Background(), st.op.ID)
	if err != nil || result.Phase != domain.ReplacementRecoveryRequired || result.IntakeDrainRequestedAt != nil || rt.drains != 1 {
		t.Fatal("rotation did not retain a recoverable boundary", result.Phase, result.Error, result.IntakeDrainRequestedAt != nil, rt.drains, err)
	}
}
