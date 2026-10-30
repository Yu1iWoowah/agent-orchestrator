package sessionmanager

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/agentsettings"
)

func TestTUISettingsRecoveryStopRetryKeepsFenceAndRedactsNativeError(t *testing.T) {
	f := newTUISettingsFixture(t, domain.HarnessCodex)
	f.runtime.destroyErr = errors.New("AO_AGENT_SETTINGS_GRANT=stop-error-canary")
	zero := int64(0)
	high := domain.AgentEffortSelection("high")
	op, err := f.control.Change(t.Context(), agentsettings.Caller{}, f.rec.ID, agentsettings.Change{ExpectedRevision: &zero, ApplyMode: domain.AgentSettingsRestart, Settings: domain.AgentSettingsPatch{Effort: &high}, IdempotencyKey: "operator-stop-retry"})
	if err != nil {
		t.Fatal(err)
	}
	op = f.wait(t, op.ID)
	created := f.runtime.created
	_, err = f.m.Kill(t.Context(), f.rec.ID)
	var public *apierr.Error
	if !errors.As(err, &public) || public.Kind != apierr.KindConflict || public.Code != "AGENT_SETTINGS_WRITER_UNCONFIRMED" || strings.Contains(err.Error(), "stop-error-canary") {
		t.Fatalf("unsafe stop failure: %v", err)
	}
	saved, err := f.store.AgentSettingsOperation(t.Context(), f.rec.ID, op.ID)
	if err != nil || saved.Phase != domain.AgentSettingsRecoveryRequired || saved.Version != op.Version {
		t.Fatalf("failed stop changed recovery: %+v %v", saved, err)
	}
	f.runtime.destroyErr = nil
	if _, err = f.m.Kill(t.Context(), f.rec.ID); err != nil {
		t.Fatal(err)
	}
	saved, err = f.store.AgentSettingsOperation(t.Context(), f.rec.ID, op.ID)
	if err != nil || saved.Phase != domain.AgentSettingsCancelled || f.runtime.created != created {
		t.Fatalf("stop retry launched or failed: %+v %v", saved, err)
	}
}

type tuiSettingsOperatorSettlement interface {
	SettleTUIAgentSettings(context.Context, domain.SessionID, string, int64, bool) (domain.AgentSettingsOperation, error)
}

func TestTUISettingsOperatorSettlementRequiresConclusiveAbsence(t *testing.T) {
	for _, scenario := range []string{"already dead", "stop exact writer", "unknown", "generation mismatch", "no stop consent", "stop failed", "stale version", "not terminated", "receipt failed"} {
		t.Run(scenario, func(t *testing.T) {
			f := newTUISettingsFixture(t, domain.HarnessCodex)
			f.runtime.createErr = errors.New("lost target launch receipt")
			zero := int64(0)
			high := domain.AgentEffortSelection("high")
			op, err := f.control.Change(t.Context(), agentsettings.Caller{}, f.rec.ID, agentsettings.Change{ExpectedRevision: &zero, ApplyMode: domain.AgentSettingsRestart, Settings: domain.AgentSettingsPatch{Effort: &high}, IdempotencyKey: "operator-settlement"})
			if err != nil {
				t.Fatal(err)
			}
			op = f.wait(t, op.ID)
			if op.Phase != domain.AgentSettingsRecoveryRequired || !op.TUI.UnconfirmedRuntime {
				t.Fatalf("fixture has no unknown writer: %+v", op)
			}
			f.runtime.createErr = nil
			if scenario != "not terminated" {
				if _, err := f.m.Kill(t.Context(), f.rec.ID); err != nil {
					t.Fatal(err)
				}
			}
			op, err = f.store.AgentSettingsOperation(t.Context(), f.rec.ID, op.ID)
			if err != nil {
				t.Fatal(err)
			}
			version := op.Version
			if scenario == "stale version" {
				version++
			}
			if scenario == "unknown" {
				f.runtime.fencedResult = ports.FencedProbeResult{Liveness: ports.FencedUnknown, Reason: ports.FencedReasonProbeFailed}
			}
			if scenario == "generation mismatch" {
				f.runtime.fencedResult = ports.FencedProbeResult{Liveness: ports.FencedUnknown, Reason: ports.FencedReasonGenerationMismatch}
			}
			if scenario == "stop exact writer" || scenario == "stop failed" || scenario == "no stop consent" {
				f.runtime.aliveByHandle[string(f.rec.ID)] = true
			}
			if scenario == "stop failed" {
				f.runtime.destroyErr = errors.New("stop still unconfirmed")
			}
			db, err := sql.Open("sqlite", filepath.Join(f.rec.Metadata.WorkspacePath, "ao.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			if scenario == "receipt failed" {
				if _, err := db.Exec(`CREATE TRIGGER fail_writer_receipt BEFORE INSERT ON session_agent_settings_writer_settlements BEGIN SELECT RAISE(ABORT,'injected receipt failure'); END;`); err != nil {
					t.Fatal(err)
				}
			}
			created, destroyed := f.runtime.created, f.runtime.destroyed
			f.runtime.fencedRefs = nil
			operator, ok := any(f.m).(tuiSettingsOperatorSettlement)
			if !ok {
				t.Fatal("operator settlement boundary missing; terminated unknown writer has no exit")
			}
			_, err = operator.SettleTUIAgentSettings(t.Context(), f.rec.ID, op.ID, version, scenario == "stop exact writer" || scenario == "stop failed")
			saved, readErr := f.store.AgentSettingsOperation(t.Context(), f.rec.ID, op.ID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			success := scenario == "already dead" || scenario == "stop exact writer"
			if (err == nil) != success || saved.TUI.TerminationVerified != success || saved.TUI.UnconfirmedRuntime == success {
				t.Fatalf("settlement changed writer proof incorrectly: error=%v saved=%+v", err, saved.TUI)
			}
			if f.runtime.created != created {
				t.Fatal("settlement launched another writer")
			}
			if !success && scenario != "stop failed" && f.runtime.destroyed != destroyed {
				t.Fatal("unqualified settlement stopped a runtime")
			}
			if success {
				blocked, err := f.store.HasAbandonedTUISettingsWriter(t.Context(), f.rec.ID)
				if err != nil || blocked || saved.Phase != domain.AgentSettingsCancelled || saved.Version != op.Version+1 {
					t.Fatalf("confirmed settlement did not release only the veto: blocked=%v operation=%+v error=%v", blocked, saved, err)
				}
				seen := map[string]bool{}
				for _, ref := range f.runtime.fencedRefs {
					if ref.SessionID != f.rec.ID || ref.Generation == "" || ref.Handle.ID == "" {
						t.Fatalf("unfenced probe: %+v", ref)
					}
					seen[ref.Generation] = true
				}
				if !seen[op.ControllerGeneration] || !seen[op.TUI.TargetGeneration] || !seen[op.ID+":rollback"] {
					t.Fatalf("settlement did not verify every possible writer: %v", seen)
				}
				var actor, cancelCode string
				var revision int64
				if err := db.QueryRow("SELECT actor_kind,operation_revision FROM session_agent_settings_writer_settlements WHERE operation_id=?", op.ID).Scan(&actor, &revision); err != nil || actor != "local_operator_unverified" || revision != saved.Version {
					t.Fatalf("missing qualified settlement receipt: actor=%q revision=%d error=%v", actor, revision, err)
				}
				if err := db.QueryRow("SELECT error_code FROM session_agent_settings_audit WHERE operation_id=? AND phase='cancelled'", op.ID).Scan(&cancelCode); err != nil || cancelCode != "AGENT_SETTINGS_SESSION_TERMINATED" {
					t.Fatalf("settlement rewrote original cancellation history: %q %v", cancelCode, err)
				}
			} else if saved.Version != op.Version {
				t.Fatal("failed settlement changed the operation version")
			}
			if scenario == "receipt failed" {
				if _, err := db.Exec("DROP TRIGGER fail_writer_receipt"); err != nil {
					t.Fatal(err)
				}
				retried, err := operator.SettleTUIAgentSettings(t.Context(), f.rec.ID, op.ID, op.Version, false)
				if err != nil || !retried.TUI.TerminationVerified || retried.Version != op.Version+1 {
					t.Fatalf("receipt failure was not safely retryable: %+v %v", retried, err)
				}
			}
		})
	}
}
