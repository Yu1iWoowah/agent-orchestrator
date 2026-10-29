package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

func recoveryStep(ctx context.Context, s *sqlite.Store, id domain.SessionID, op string, version int64, step domain.AgentSettingsCheckpoint, owner domain.AgentSettingsOwner) (domain.AgentSettingsOperation, error) {
	current, err := s.SessionAgentSettings(ctx, id)
	if err != nil {
		return domain.AgentSettingsOperation{}, err
	}
	return s.AdvanceAgentSettingsRecovery(ctx, id, op, version, step, domain.AgentSettingsRecovery{Owner: owner, Settings: current.Settings, Revision: current.Revision}, time.Now())
}

func TestAgentSettingsReplacementOwnerCanFinishRecovery(t *testing.T) {
	for _, crash := range []bool{false, true} {
		name := "commit conflict"
		if crash {
			name = "crash and reopen"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s, db, rec := settingsFixture(t)
			if _, err := s.CreateConversation(ctx, "recovery-conv", domain.ConversationScopeSession, rec.ProjectID, rec.ID, time.Now()); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec("UPDATE sessions SET provider_conversation_id='native-1' WHERE id=?", rec.ID); err != nil {
				t.Fatal(err)
			}
			op := settingsOperation(rec.ID)
			if _, err := s.BeginAgentSettingsOperation(ctx, op); err != nil {
				t.Fatal(err)
			}
			started, err := s.AdvanceAgentSettingsOperation(ctx, rec.ID, op.ID, 0, domain.AgentSettingsApplyingModel, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec("UPDATE sessions SET controller_generation='replacement',provider_conversation_id='native-2',model='durable-current' WHERE id=?", rec.ID); err != nil {
				t.Fatal(err)
			}
			if crash {
				var seq int
				var name, path string
				if err := db.QueryRow("PRAGMA database_list").Scan(&seq, &name, &path); err != nil {
					t.Fatal(err)
				}
				reopened, err := sqlite.Open(filepath.Dir(path))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = reopened.Close() })
				s = reopened
			} else {
				if _, err := s.CommitAgentSettingsOperation(ctx, rec.ID, op.ID, time.Now()); !errors.Is(err, ports.ErrAgentSettingsConflict) {
					t.Fatalf("stale commit: %v", err)
				}
			}
			pending, err := s.AgentSettingsOperation(ctx, rec.ID, op.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !crash && pending.Phase != domain.AgentSettingsRecoveryRequired {
				t.Fatalf("phase=%s", pending.Phase)
			}
			if crash && pending.Version != started.Version {
				t.Fatalf("crash lost checkpoint: %+v", pending)
			}
			current, err := s.SessionAgentSettings(ctx, rec.ID)
			if err != nil {
				t.Fatal(err)
			}
			owner := domain.AgentSettingsOwner{SessionID: rec.ID, AccountID: current.AccountID, ControllerGeneration: current.OwnerGeneration, ProviderConversationID: current.ProviderConversationID, BranchID: current.BranchID}
			rolling, err := recoveryStep(ctx, s, rec.ID, op.ID, pending.Version, domain.AgentSettingsRollingBack, owner)
			if err != nil {
				t.Fatalf("replacement rolling_back permanently blocked: %v", err)
			}
			if rolling.Recovery == nil || rolling.Recovery.Owner != owner || rolling.Recovery.Settings != current.Settings || rolling.Source != op.Source || rolling.Target != op.Target {
				t.Fatalf("recovery lost executor/current preference/original intent: %+v", rolling)
			}
			restored, err := recoveryStep(ctx, s, rec.ID, op.ID, rolling.Version, domain.AgentSettingsRestored, owner)
			if err != nil {
				t.Fatalf("replacement restored blocked: %v", err)
			}
			ended, err := s.FinishAgentSettingsOperation(ctx, rec.ID, op.ID, restored.Version, domain.AgentSettingsFailed, domain.AgentSettingsErrorConflict, time.Now())
			if err != nil || ended.Phase != domain.AgentSettingsFailed {
				t.Fatalf("finish recovery: %+v %v", ended, err)
			}
			still, err := s.SessionAgentSettings(ctx, rec.ID)
			if err != nil || still.Settings != current.Settings || still.Revision != current.Revision {
				t.Fatalf("recovery rewrote current durable preference: %+v %v", still, err)
			}
			next := settingsOperation(rec.ID)
			next.ID, next.IdempotencyKey = "next-operation", "next-key"
			next.Source, next.BaseRevision, next.ControllerGeneration = current.Settings, current.Revision, current.OwnerGeneration
			if _, err := s.BeginAgentSettingsOperation(ctx, next); err != nil {
				t.Fatalf("recovery still owns pending slot: %v", err)
			}
		})
	}
}

func TestAgentSettingsRecoveryExecutorIsFencedAndAuditAtomic(t *testing.T) {
	ctx := context.Background()
	s, db, rec := settingsFixture(t)
	if _, err := s.CreateConversation(ctx, "recovery-fence", domain.ConversationScopeSession, rec.ProjectID, rec.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE sessions SET provider_conversation_id='native-1' WHERE id=?", rec.ID); err != nil {
		t.Fatal(err)
	}
	op := settingsOperation(rec.ID)
	op.RequestID = "recovery-request"
	if _, err := s.BeginAgentSettingsOperation(ctx, op); err != nil {
		t.Fatal(err)
	}
	started, err := s.AdvanceAgentSettingsOperation(ctx, rec.ID, op.ID, 0, domain.AgentSettingsApplyingFast, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE sessions SET controller_generation='new-owner',provider_conversation_id='native-2' WHERE id=?", rec.ID); err != nil {
		t.Fatal(err)
	}
	current, err := s.SessionAgentSettings(ctx, rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	owner := domain.AgentSettingsOwner{SessionID: rec.ID, AccountID: current.AccountID, ControllerGeneration: current.OwnerGeneration, ProviderConversationID: current.ProviderConversationID, BranchID: current.BranchID}
	stale := owner
	stale.ControllerGeneration = "generation-1"
	if _, err := recoveryStep(ctx, s, rec.ID, op.ID, started.Version, domain.AgentSettingsRollingBack, stale); !errors.Is(err, ports.ErrAgentSettingsConflict) {
		t.Fatalf("stale executor accepted: %v", err)
	}
	if _, err := recoveryStep(ctx, s, rec.ID, op.ID, started.Version-1, domain.AgentSettingsRollingBack, owner); !errors.Is(err, ports.ErrAgentSettingsConflict) {
		t.Fatalf("stale operation version accepted: %v", err)
	}
	if _, err := db.Exec("CREATE TRIGGER recovery_audit_fault BEFORE INSERT ON session_agent_settings_audit WHEN NEW.phase='recovery_required' BEGIN SELECT RAISE(ABORT,'recovery-audit-injected'); END"); err != nil {
		t.Fatal(err)
	}
	if _, err := recoveryStep(ctx, s, rec.ID, op.ID, started.Version, domain.AgentSettingsRollingBack, owner); err == nil {
		t.Fatal("recovery audit fault ignored")
	}
	unchanged, err := s.AgentSettingsOperation(ctx, rec.ID, op.ID)
	if err != nil || unchanged.Version != started.Version || unchanged.Phase != domain.AgentSettingsPending || unchanged.Recovery != nil {
		t.Fatalf("partial recovery claim: %+v %v", unchanged, err)
	}
	if _, err := db.Exec("DROP TRIGGER recovery_audit_fault"); err != nil {
		t.Fatal(err)
	}
	rolling, err := recoveryStep(ctx, s, rec.ID, op.ID, started.Version, domain.AgentSettingsRollingBack, owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE sessions SET controller_generation='third-owner',provider_conversation_id='native-3' WHERE id=?", rec.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := recoveryStep(ctx, s, rec.ID, op.ID, rolling.Version, domain.AgentSettingsRestored, owner); !errors.Is(err, ports.ErrAgentSettingsConflict) {
		t.Fatalf("replaced executor claimed restored: %v", err)
	}
	owner.ControllerGeneration, owner.ProviderConversationID = "third-owner", "native-3"
	rolling, err = recoveryStep(ctx, s, rec.ID, op.ID, rolling.Version, domain.AgentSettingsRollingBack, owner)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := recoveryStep(ctx, s, rec.ID, op.ID, rolling.Version, domain.AgentSettingsRestored, owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE sessions SET model='new-durable-preference' WHERE id=?", rec.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinishAgentSettingsOperation(ctx, rec.ID, op.ID, restored.Version, domain.AgentSettingsFailed, domain.AgentSettingsErrorRecovery, time.Now()); !errors.Is(err, ports.ErrAgentSettingsConflict) {
		t.Fatalf("stale restored preference released fence: %v", err)
	}
	rolling, err = recoveryStep(ctx, s, rec.ID, op.ID, restored.Version, domain.AgentSettingsRollingBack, owner)
	if err != nil {
		t.Fatal(err)
	}
	restored, err = recoveryStep(ctx, s, rec.ID, op.ID, rolling.Version, domain.AgentSettingsRestored, owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE sessions SET is_terminated=1 WHERE id=?", rec.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinishAgentSettingsOperation(ctx, rec.ID, op.ID, restored.Version, domain.AgentSettingsFailed, domain.AgentSettingsErrorRecovery, time.Now()); !errors.Is(err, ports.ErrAgentSettingsConflict) {
		t.Fatalf("terminated owner released fence: %v", err)
	}
	if _, err := db.Exec("UPDATE sessions SET is_terminated=0 WHERE id=?", rec.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinishAgentSettingsOperation(ctx, rec.ID, op.ID, restored.Version, domain.AgentSettingsFailed, domain.AgentSettingsErrorRecovery, time.Now()); err != nil {
		t.Fatal(err)
	}
	var audits int
	if err := db.QueryRow("SELECT COUNT(*) FROM session_agent_settings_audit WHERE operation_id=? AND phase='recovery_required' AND request_id='recovery-request'", op.ID).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("recovery audit duplication: %d %v", audits, err)
	}
}
