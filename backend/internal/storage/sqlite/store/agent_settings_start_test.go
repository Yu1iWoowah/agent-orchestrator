package store_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

func settingsStartFixture(t *testing.T, missing string) (*sqlite.Store, *sql.DB, domain.SessionRecord) {
	t.Helper()
	s, db, rec := settingsFixture(t)
	if missing == "tui" {
		if _, err := db.Exec("UPDATE sessions SET session_mode='tui',runtime_launch_id='generation-1' WHERE id=?", rec.ID); err != nil {
			t.Fatal(err)
		}
		return s, db, rec
	}
	if missing != "branch" {
		if _, err := s.CreateConversation(t.Context(), "start-context", domain.ConversationScopeSession, rec.ProjectID, rec.ID, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if missing != "native" {
		if _, err := db.Exec("UPDATE sessions SET provider_conversation_id='native-1' WHERE id=?", rec.ID); err != nil {
			t.Fatal(err)
		}
	}
	if missing == "generation" {
		if _, err := db.Exec("UPDATE sessions SET controller_generation='' WHERE id=?", rec.ID); err != nil {
			t.Fatal(err)
		}
	}
	if missing == "account" {
		if _, err := db.Exec("DELETE FROM session_agent_accounts WHERE session_id=?", rec.ID); err != nil {
			t.Fatal(err)
		}
	}
	return s, db, rec
}

func TestAgentSettingsUnrecoverableOwnerCannotStart(t *testing.T) {
	for _, missing := range []string{"tui", "generation", "account", "native", "branch"} {
		for _, terminal := range []domain.AgentSettingsPhase{domain.AgentSettingsCancelled, domain.AgentSettingsFailed} {
			t.Run(missing+"/"+string(terminal), func(t *testing.T) {
				ctx := context.Background()
				s, db, rec := settingsStartFixture(t, missing)
				current, err := s.SessionAgentSettings(ctx, rec.ID)
				if err != nil {
					t.Fatal(err)
				}
				op := settingsOperation(rec.ID)
				op.AccountID, op.ControllerGeneration = current.AccountID, current.OwnerGeneration
				queued, err := s.BeginAgentSettingsOperation(ctx, op)
				if err != nil {
					t.Fatal(err)
				}
				var before int
				if err := db.QueryRow("SELECT COUNT(*) FROM change_log WHERE session_id=?", rec.ID).Scan(&before); err != nil {
					t.Fatal(err)
				}
				for _, step := range []domain.AgentSettingsCheckpoint{domain.AgentSettingsApplyingModel, domain.AgentSettingsApplyingEffort, domain.AgentSettingsApplyingFast, domain.AgentSettingsVerifying} {
					_, err := s.AdvanceAgentSettingsOperation(ctx, rec.ID, op.ID, queued.Version, step, time.Now())
					if !errors.Is(err, ports.ErrAgentSettingsConflict) || !strings.Contains(err.Error(), "settings_owner_unrecoverable") {
						t.Fatalf("%s owner entered %s without recovery support: %v", missing, step, err)
					}
				}
				if _, err := s.FinishAgentSettingsOperation(ctx, rec.ID, op.ID, queued.Version, domain.AgentSettingsRecoveryRequired, domain.AgentSettingsErrorProvider, time.Now()); !errors.Is(err, ports.ErrAgentSettingsConflict) {
					t.Fatalf("unstarted owner entered recovery: %v", err)
				}
				still, err := s.AgentSettingsOperation(ctx, rec.ID, op.ID)
				if err != nil || still.Phase != domain.AgentSettingsPending || still.Checkpoint != domain.AgentSettingsQueued || still.StartedAt != nil || still.Version != queued.Version {
					t.Fatalf("rejected start changed journal: %+v %v", still, err)
				}
				var after int
				if err := db.QueryRow("SELECT COUNT(*) FROM change_log WHERE session_id=?", rec.ID).Scan(&after); err != nil || before != after {
					t.Fatalf("rejected start emitted CDC: %d -> %d, %v", before, after, err)
				}
				category := domain.AgentSettingsErrorValidation
				if terminal == domain.AgentSettingsCancelled {
					category = domain.AgentSettingsErrorCancelled
				}
				if _, err := s.FinishAgentSettingsOperation(ctx, rec.ID, op.ID, queued.Version, terminal, category, time.Now()); err != nil {
					t.Fatalf("cannot safely end queued operation: %v", err)
				}
				op.ID, op.IdempotencyKey = "next-op", "next-key"
				if _, err := s.BeginAgentSettingsOperation(ctx, op); err != nil {
					t.Fatalf("rejected start permanently blocked Begin: %v", err)
				}
			})
		}
	}
}

func TestAgentSettingsRecoveryEntryRejectsUnrecoverableOwner(t *testing.T) {
	for _, missing := range []string{"tui", "generation", "account", "native", "branch"} {
		t.Run(missing, func(t *testing.T) {
			ctx := context.Background()
			s, db, rec := settingsStartFixture(t, missing)
			current, err := s.SessionAgentSettings(ctx, rec.ID)
			if err != nil {
				t.Fatal(err)
			}
			op := settingsOperation(rec.ID)
			op.AccountID, op.ControllerGeneration = current.AccountID, current.OwnerGeneration
			if _, err := s.BeginAgentSettingsOperation(ctx, op); err != nil {
				t.Fatal(err)
			}
			// Synthesize an already-started row accepted before the start guard.
			if _, err := db.Exec("UPDATE session_agent_settings_operations SET checkpoint='applying_model',started_at=? WHERE id=?", time.Now(), op.ID); err != nil {
				t.Fatal(err)
			}
			_, err = s.FinishAgentSettingsOperation(ctx, rec.ID, op.ID, 0, domain.AgentSettingsRecoveryRequired, domain.AgentSettingsErrorProvider, time.Now())
			if !errors.Is(err, ports.ErrAgentSettingsConflict) || !strings.Contains(err.Error(), "settings_owner_unrecoverable") {
				t.Fatalf("unrecoverable owner entered recovery_required: %v", err)
			}
			still, err := s.AgentSettingsOperation(ctx, rec.ID, op.ID)
			if err != nil || still.Phase != domain.AgentSettingsPending || still.Version != 0 || still.StartedAt == nil {
				t.Fatalf("unsupported recovery changed fence: %+v %v", still, err)
			}
		})
	}
}
