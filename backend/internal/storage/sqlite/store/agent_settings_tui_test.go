package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestTUISettingsRestartClaimsHandoffAtomically(t *testing.T) {
	s, db, rec := settingsFixture(t)
	ctx := context.Background()
	if _, err := db.Exec("UPDATE sessions SET session_mode='tui',runtime_launch_id='source',runtime_handle_id='runtime',agent_session_id='native',agent_session_id_launch_id='source' WHERE id=?", rec.ID); err != nil {
		t.Fatal(err)
	}
	current, err := s.SessionAgentSettings(ctx, rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	op := settingsOperation(rec.ID)
	op.ApplyMode = domain.AgentSettingsRestart
	op.AccountID, op.ControllerGeneration = current.AccountID, current.OwnerGeneration
	op.ProviderConversationID = current.ProviderConversationID
	accepted, err := s.BeginAgentSettingsOperation(ctx, op)
	if err != nil {
		t.Fatal(err)
	}
	transition, found, err := s.GetActiveSessionInterfaceTransition(ctx, rec.ID)
	if err != nil || !found || transition.ID != accepted.ID || transition.SourceMode != domain.SessionModeTUI || transition.TargetMode != domain.SessionModeTUI || transition.NativeConversationID != "native" {
		t.Fatalf("restart must atomically own native handoff: %+v found=%v error=%v", transition, found, err)
	}
	if _, _, err := s.CreateSessionInterfaceTransition(ctx, domain.SessionInterfaceTransition{ID: "account-move", SessionID: rec.ID, SourceMode: domain.SessionModeTUI, TargetMode: domain.SessionModeTUI, SourceAccountID: current.AccountID, TargetAccountID: "managed", Policy: domain.SessionInterfaceTransitionDrain, HistoryPolicy: domain.SessionInterfaceTransitionHistoryStrict, Phase: domain.SessionInterfaceTransitionRequested, CreatedAt: time.Now(), UpdatedAt: time.Now()}); !errors.Is(err, ports.ErrAgentSettingsConflict) {
		t.Fatalf("account move raced settings: %v", err)
	}
	still, err := s.SessionAgentSettings(ctx, rec.ID)
	if err != nil || still.Settings != current.Settings || still.Revision != current.Revision {
		t.Fatalf("claim changed preferences: %+v %v", still, err)
	}
}

func TestTUISettingsCannotCommitBeforeVerifiedTarget(t *testing.T) {
	s, db, rec := settingsFixture(t)
	ctx := t.Context()
	if _, err := db.Exec("UPDATE sessions SET session_mode='tui',runtime_launch_id='source',runtime_handle_id='runtime',agent_session_id='native',agent_session_id_launch_id='source' WHERE id=?", rec.ID); err != nil {
		t.Fatal(err)
	}
	current, err := s.SessionAgentSettings(ctx, rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	op := settingsOperation(rec.ID)
	op.ApplyMode = domain.AgentSettingsRestart
	op.AccountID, op.ControllerGeneration, op.ProviderConversationID = current.AccountID, current.OwnerGeneration, current.ProviderConversationID
	op, err = s.BeginAgentSettingsOperation(ctx, op)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitAgentSettingsOperation(ctx, rec.ID, op.ID, time.Now()); !errors.Is(err, ports.ErrAgentSettingsConflict) {
		t.Fatalf("queued restart committed before target existed: %v", err)
	}
	execution := domain.AgentSettingsExecution{Owner: domain.AgentSettingsOwner{SessionID: rec.ID, AccountID: current.AccountID, ControllerGeneration: current.OwnerGeneration, ProviderConversationID: current.ProviderConversationID, BranchID: current.BranchID}, Revision: current.Revision, Qualification: domain.AgentSettingsQualification{InputFingerprint: "test-inputs", PolicyVersion: "test-policy"}, Plan: domain.AgentSettingsProviderPlan{Protocol: "fake-native-resume", ProviderConversationID: current.ProviderConversationID, Source: op.Source, Target: op.Target, Before: domain.AgentSettingsNativeValues{Model: string(op.Source.Model)}, After: domain.AgentSettingsNativeValues{Model: string(op.Target.Model)}}}
	op, err = s.PinAgentSettingsExecution(ctx, rec.ID, op.ID, op.Version, execution, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	op, err = s.AdvanceAgentSettingsOperation(ctx, rec.ID, op.ID, op.Version, domain.AgentSettingsApplyingModel, time.Now())
	if err != nil {
		t.Fatalf("linked fully identified TUI cannot begin: %v", err)
	}
	for _, cp := range []domain.AgentSettingsCheckpoint{domain.AgentSettingsVerifying, domain.AgentSettingsVerified} {
		op, err = s.AdvanceAgentSettingsOperation(ctx, rec.ID, op.ID, op.Version, cp, time.Now())
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.CommitAgentSettingsOperation(ctx, rec.ID, op.ID, time.Now()); !errors.Is(err, ports.ErrAgentSettingsConflict) {
		t.Fatalf("source generation accepted as target: %v", err)
	}
	if _, err := db.Exec("UPDATE sessions SET runtime_launch_id=?,agent_session_id_launch_id=? WHERE id=?", op.TUI.TargetGeneration, op.TUI.TargetGeneration, rec.ID); err != nil {
		t.Fatal(err)
	}
	applied, err := s.CommitAgentSettingsOperation(ctx, rec.ID, op.ID, time.Now())
	if err != nil || applied.Phase != domain.AgentSettingsApplied {
		t.Fatalf("verified target failed commit: %+v %v", applied, err)
	}
	if _, active, err := s.GetActiveSessionInterfaceTransition(ctx, rec.ID); err != nil || active {
		t.Fatalf("commit left active handoff: %v %v", active, err)
	}
}

func TestTUISettingsCancelReleasesBothClaims(t *testing.T) {
	s, db, rec := settingsFixture(t)
	ctx := t.Context()
	if _, err := db.Exec("UPDATE sessions SET session_mode='tui',runtime_launch_id='source',runtime_handle_id='runtime',agent_session_id='native',agent_session_id_launch_id='source' WHERE id=?", rec.ID); err != nil {
		t.Fatal(err)
	}
	current, err := s.SessionAgentSettings(ctx, rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	op := settingsOperation(rec.ID)
	op.ApplyMode = domain.AgentSettingsRestart
	op.AccountID, op.ControllerGeneration, op.ProviderConversationID = current.AccountID, current.OwnerGeneration, current.ProviderConversationID
	op, err = s.BeginAgentSettingsOperation(ctx, op)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.FinishAgentSettingsOperation(ctx, rec.ID, op.ID, op.Version, domain.AgentSettingsCancelled, domain.AgentSettingsErrorCancelled, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, active, err := s.GetActiveSessionInterfaceTransition(ctx, rec.ID); err != nil || active {
		t.Fatalf("cancel left handoff fence: %v %v", active, err)
	}
	op.ID, op.IdempotencyKey, op.TUI = "next", "next", nil
	if _, err := s.BeginAgentSettingsOperation(ctx, op); err != nil {
		t.Fatalf("cancel blocked next intent: %v", err)
	}
}
