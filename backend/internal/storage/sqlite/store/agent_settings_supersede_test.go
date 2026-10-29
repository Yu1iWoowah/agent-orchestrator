package store_test

import (
	"errors"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestLocalSettingsSupersedeIsAtomicAndPreservesAcceptedIntent(t *testing.T) {
	s, db, rec := settingsFixture(t)
	old, err := s.BeginAgentSettingsOperation(t.Context(), settingsOperation(rec.ID))
	if err != nil {
		t.Fatal(err)
	}
	next := old
	next.ID, next.IdempotencyKey = "replacement", "replacement-key"
	next.Target.FastMode = domain.AgentFastOn
	if _, err := db.Exec(`CREATE TRIGGER fail_replacement_audit BEFORE INSERT ON session_agent_settings_audit
WHEN NEW.operation_id='replacement' BEGIN SELECT RAISE(ABORT,'replacement audit fault'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SupersedeQueuedAgentSettingsOperation(t.Context(), old.ID, old.Version, next); err == nil {
		t.Fatal("supersede ignored audit failure")
	}
	retained, err := s.AgentSettingsOperation(t.Context(), rec.ID, old.ID)
	if err != nil || retained.Phase != domain.AgentSettingsPending || retained.Version != old.Version || retained.Target != old.Target {
		t.Fatalf("failed replacement lost original pending: %+v %v", retained, err)
	}
	if _, err := s.AgentSettingsOperation(t.Context(), rec.ID, next.ID); !errors.Is(err, ports.ErrAgentSettingsNotFound) {
		t.Fatalf("failed replacement left partial new intent: %v", err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM session_agent_settings_audit`).Scan(&count); err != nil || count != 1 {
		t.Fatal("failed replacement leaked an audit event", count, err)
	}
	if _, err := db.Exec(`DROP TRIGGER fail_replacement_audit`); err != nil {
		t.Fatal(err)
	}
	accepted, err := s.SupersedeQueuedAgentSettingsOperation(t.Context(), old.ID, old.Version, next)
	if err != nil || accepted.ID != next.ID {
		t.Fatal("replacement failed", err)
	}
	retained, found, err := s.AgentSettingsOperationByKey(t.Context(), rec.ID, old.IdempotencyKey)
	if err != nil || !found || retained.Phase != domain.AgentSettingsCancelled || retained.Target != old.Target || retained.ErrorCode != "AGENT_SETTINGS_SUPERSEDED" {
		t.Fatalf("supersede rewrote accepted request: %+v %v", retained, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM session_agent_settings_audit`).Scan(&count); err != nil || count != 3 {
		t.Fatal("atomic supersede audit incomplete", count, err)
	}
}
