package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestAgentSettingsConflictTerminatesUnstartedPending(t *testing.T) {
	ctx := context.Background()
	s, db, rec := settingsFixture(t)
	op := settingsOperation(rec.ID)
	if _, err := s.BeginAgentSettingsOperation(ctx, op); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE sessions SET controller_generation='generation-2' WHERE id=?", rec.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitAgentSettingsOperation(ctx, rec.ID, op.ID, time.Now()); !errors.Is(err, ports.ErrAgentSettingsConflict) {
		t.Fatalf("stale commit: %v", err)
	}
	ended, err := s.AgentSettingsOperation(ctx, rec.ID, op.ID)
	if err != nil || ended.Phase != domain.AgentSettingsFailed {
		t.Fatalf("conflicting operation still blocks session: phase=%s err=%v", ended.Phase, err)
	}
	op.ID, op.IdempotencyKey, op.ControllerGeneration = "op-2", "key-2", "generation-2"
	if _, err := s.BeginAgentSettingsOperation(ctx, op); err != nil {
		t.Fatalf("ended operation did not release pending slot: %v", err)
	}
}

func TestAgentSettingsJournalMetadataSurvivesReplay(t *testing.T) {
	ctx := context.Background()
	s, _, rec := settingsFixture(t)
	op := settingsOperation(rec.ID)
	raw, err := json.Marshal(op)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	wire["applyMode"], wire["requestId"] = "after-turn", "request-first"
	raw, _ = json.Marshal(wire)
	if err := json.Unmarshal(raw, &op); err != nil {
		t.Fatal(err)
	}
	// IdempotencyKey is intentionally not a public JSON field.
	op.IdempotencyKey = "journal-key"
	got, err := s.BeginAgentSettingsOperation(ctx, op)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(got)
	wire = nil
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if wire["applyMode"] != "after-turn" || wire["requestId"] != "request-first" {
		t.Fatalf("journal metadata lost: %s", raw)
	}
}

func TestAgentSettingsConflictAuditFailureKeepsPending(t *testing.T) {
	ctx := context.Background()
	s, db, rec := settingsFixture(t)
	op := settingsOperation(rec.ID)
	if _, err := s.BeginAgentSettingsOperation(ctx, op); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE sessions SET controller_generation='generation-2' WHERE id=?", rec.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TRIGGER settings_end_fault BEFORE INSERT ON session_agent_settings_audit WHEN NEW.phase='failed' BEGIN SELECT RAISE(ABORT,'end-audit-injected'); END"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitAgentSettingsOperation(ctx, rec.ID, op.ID, time.Now()); err == nil || errors.Is(err, ports.ErrAgentSettingsConflict) {
		t.Fatalf("terminal audit fault was not surfaced: %v", err)
	}
	got, err := s.AgentSettingsOperation(ctx, rec.ID, op.ID)
	if err != nil || got.Phase != domain.AgentSettingsPending {
		t.Fatalf("partial terminal write: %+v %v", got, err)
	}
}

func TestAgentSettingsContextUsesPrimaryProjectConversation(t *testing.T) {
	ctx := context.Background()
	s, db, rec := settingsFixture(t)
	at := time.Now()
	primary, err := s.CreateConversation(ctx, "project-primary", domain.ConversationScopeProject, rec.ProjectID, rec.ID, at)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO conversations(id,scope,project_id,session_id,current_session_id,active_branch_id,created_at,updated_at) VALUES('provisioning-leftover','session',?,?,?,'leftover:root',?,?)", rec.ProjectID, rec.ID, rec.ID, at.Add(time.Minute), at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO conversation_branches(id,conversation_id,session_id,provider_conversation_id,created_at) VALUES('leftover:root','provisioning-leftover',?,'',?)", rec.ID, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	got, err := s.SessionAgentSettings(ctx, rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.BranchID != primary.ActiveBranchID {
		t.Fatalf("settings borrowed non-primary branch: got=%s primary=%s", got.BranchID, primary.ActiveBranchID)
	}
}

func TestAgentSettingsCancelIsAtomicReplayAndReleasesSlot(t *testing.T) {
	ctx := context.Background()
	s, db, rec := settingsFixture(t)
	op := settingsOperation(rec.ID)
	op.RequestID = "request-original"
	if _, err := s.BeginAgentSettingsOperation(ctx, op); err != nil {
		t.Fatal(err)
	}
	at := op.CreatedAt.Add(time.Minute)
	ended, err := s.FinishAgentSettingsOperation(ctx, rec.ID, op.ID, 0, domain.AgentSettingsCancelled, domain.AgentSettingsErrorCancelled, at)
	if err != nil || ended.Version != 1 || ended.FinishedAt == nil || !ended.FinishedAt.Equal(at) {
		t.Fatalf("cancel: %+v %v", ended, err)
	}
	retry, err := s.FinishAgentSettingsOperation(ctx, rec.ID, op.ID, 0, domain.AgentSettingsCancelled, domain.AgentSettingsErrorCancelled, at.Add(time.Minute))
	if err != nil || retry.Version != 1 || !retry.UpdatedAt.Equal(at) {
		t.Fatalf("cancel retry: %+v %v", retry, err)
	}
	op.RequestID = "request-retry"
	replayed, err := s.BeginAgentSettingsOperation(ctx, op)
	if err != nil || replayed.RequestID != "request-original" || replayed.Phase != domain.AgentSettingsCancelled {
		t.Fatalf("replay identity: %+v %v", replayed, err)
	}
	var audits int
	if err := db.QueryRow("SELECT COUNT(*) FROM session_agent_settings_audit WHERE operation_id=? AND phase='cancelled' AND error_category='cancelled' AND request_id='request-original'", op.ID).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("cancel audit=%d %v", audits, err)
	}
	if _, found, err := s.PendingAgentSettingsOperation(ctx, rec.ID); err != nil || found {
		t.Fatalf("cancel still pending: %v %v", found, err)
	}
	op.ID, op.IdempotencyKey = "op-next", "key-next"
	if _, err := s.BeginAgentSettingsOperation(ctx, op); err != nil {
		t.Fatalf("slot not released: %v", err)
	}
}

func TestAgentSettingsStartedOutcomeRemainsFencedUntilRestored(t *testing.T) {
	ctx := context.Background()
	s, db, rec := settingsFixture(t)
	var beforeEvents int
	if err := db.QueryRow("SELECT COUNT(*) FROM change_log WHERE session_id=? AND event_type='session_updated'", rec.ID).Scan(&beforeEvents); err != nil {
		t.Fatal(err)
	}
	op := settingsOperation(rec.ID)
	if _, err := s.BeginAgentSettingsOperation(ctx, op); err != nil {
		t.Fatal(err)
	}
	at := op.CreatedAt.Add(time.Minute)
	started, err := s.AdvanceAgentSettingsOperation(ctx, rec.ID, op.ID, 0, domain.AgentSettingsApplyingModel, at)
	if err != nil || started.Version != 1 || started.StartedAt == nil {
		t.Fatalf("start checkpoint: %+v %v", started, err)
	}
	if _, err := s.AdvanceAgentSettingsOperation(ctx, rec.ID, op.ID, 0, domain.AgentSettingsApplyingModel, at.Add(time.Minute)); !errors.Is(err, ports.ErrAgentSettingsConflict) {
		t.Fatalf("old checkpoint CAS authorized a duplicate action: %v", err)
	}
	if _, err := s.AdvanceAgentSettingsOperation(ctx, rec.ID, op.ID, 0, domain.AgentSettingsApplyingEffort, at); !errors.Is(err, ports.ErrAgentSettingsConflict) {
		t.Fatalf("stale CAS: %v", err)
	}
	for _, phase := range []domain.AgentSettingsPhase{domain.AgentSettingsCancelled, domain.AgentSettingsFailed} {
		category := domain.AgentSettingsErrorProvider
		if phase == domain.AgentSettingsCancelled {
			category = domain.AgentSettingsErrorCancelled
		}
		if _, err := s.FinishAgentSettingsOperation(ctx, rec.ID, op.ID, 1, phase, category, at); !errors.Is(err, ports.ErrAgentSettingsConflict) {
			t.Fatalf("uncertain work freed by %s: %v", phase, err)
		}
	}
	recovery, err := s.FinishAgentSettingsOperation(ctx, rec.ID, op.ID, 1, domain.AgentSettingsRecoveryRequired, domain.AgentSettingsErrorProvider, at)
	if err != nil || recovery.Version != 2 || recovery.FinishedAt != nil {
		t.Fatalf("recovery: %+v %v", recovery, err)
	}
	op.ID, op.IdempotencyKey = "op-next", "key-next"
	if _, err := s.BeginAgentSettingsOperation(ctx, op); !errors.Is(err, ports.ErrAgentSettingsConflict) {
		t.Fatalf("uncertain fence released: %v", err)
	}
	if _, err := s.AdvanceAgentSettingsOperation(ctx, rec.ID, "op-1", 2, domain.AgentSettingsApplyingEffort, at); !errors.Is(err, ports.ErrAgentSettingsConflict) {
		t.Fatalf("resumed applying uncertain operation: %v", err)
	}
	if _, err := s.AdvanceAgentSettingsOperation(ctx, rec.ID, "op-1", 2, domain.AgentSettingsRollingBack, at); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdvanceAgentSettingsOperation(ctx, rec.ID, "op-1", 3, domain.AgentSettingsRestored, at); err != nil {
		t.Fatal(err)
	}
	if ended, err := s.FinishAgentSettingsOperation(ctx, rec.ID, "op-1", 4, domain.AgentSettingsFailed, domain.AgentSettingsErrorProvider, at); err != nil || ended.Version != 5 {
		t.Fatalf("restored outcome: %+v %v", ended, err)
	}
	if _, err := s.BeginAgentSettingsOperation(ctx, op); err != nil {
		t.Fatalf("restored operation blocks next: %v", err)
	}
	var afterEvents int
	if err := db.QueryRow("SELECT COUNT(*) FROM change_log WHERE session_id=? AND event_type='session_updated'", rec.ID).Scan(&afterEvents); err != nil || afterEvents-beforeEvents != 7 {
		t.Fatalf("checkpoint/phase CDC duplicated or missing: delta=%d err=%v", afterEvents-beforeEvents, err)
	}
}

func TestAgentSettingsStartedOwnerConflictRequiresRecovery(t *testing.T) {
	ctx := context.Background()
	s, db, rec := settingsFixture(t)
	op := settingsOperation(rec.ID)
	if _, err := s.BeginAgentSettingsOperation(ctx, op); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdvanceAgentSettingsOperation(ctx, rec.ID, op.ID, 0, domain.AgentSettingsApplyingModel, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE sessions SET controller_generation='replacement' WHERE id=?", rec.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitAgentSettingsOperation(ctx, rec.ID, op.ID, time.Now()); !errors.Is(err, ports.ErrAgentSettingsConflict) {
		t.Fatalf("owner conflict: %v", err)
	}
	got, found, err := s.PendingAgentSettingsOperation(ctx, rec.ID)
	if err != nil || !found || got.Phase != domain.AgentSettingsRecoveryRequired || got.ErrorCategory != domain.AgentSettingsErrorConflict {
		t.Fatalf("uncertain owner did not enter recovery: %+v %v %v", got, found, err)
	}
}

func TestAgentSettingsCheckpointAndTerminalFaultsRollback(t *testing.T) {
	ctx := context.Background()
	s, db, rec := settingsFixture(t)
	op := settingsOperation(rec.ID)
	if _, err := s.BeginAgentSettingsOperation(ctx, op); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TRIGGER checkpoint_fault BEFORE UPDATE OF checkpoint ON session_agent_settings_operations BEGIN SELECT RAISE(ABORT,'checkpoint-injected'); END"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdvanceAgentSettingsOperation(ctx, rec.ID, op.ID, 0, domain.AgentSettingsApplyingFast, time.Now()); err == nil {
		t.Fatal("checkpoint fault ignored")
	}
	got, err := s.AgentSettingsOperation(ctx, rec.ID, op.ID)
	if err != nil || got.Version != 0 || got.StartedAt != nil || got.Checkpoint != domain.AgentSettingsQueued {
		t.Fatalf("partial checkpoint: %+v %v", got, err)
	}
	if _, err := db.Exec("DROP TRIGGER checkpoint_fault"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TRIGGER finish_fault BEFORE INSERT ON session_agent_settings_audit WHEN NEW.phase='failed' BEGIN SELECT RAISE(ABORT,'finish-injected'); END"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinishAgentSettingsOperation(ctx, rec.ID, op.ID, 0, domain.AgentSettingsFailed, domain.AgentSettingsErrorValidation, time.Now()); err == nil {
		t.Fatal("finish audit fault ignored")
	}
	got, err = s.AgentSettingsOperation(ctx, rec.ID, op.ID)
	if err != nil || got.Version != 0 || got.Phase != domain.AgentSettingsPending || got.FinishedAt != nil {
		t.Fatalf("partial terminal outcome: %+v %v", got, err)
	}
}
