package store_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

func settingsFixture(t *testing.T) (*sqlite.Store, *sql.DB, domain.SessionRecord) {
	t.Helper()
	dir := t.TempDir()
	s := sqlitetest.MustOpenAt(t, dir)
	db, err := sql.Open("sqlite", filepath.Join(dir, "ao.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	seedProject(t, s, "settings")
	rec := sampleRecord("settings")
	rec.Metadata.Model = "old-model"
	rec.Metadata.Effort = "high"
	rec.Metadata.ControllerGeneration = "generation-1"
	rec.Mode = domain.SessionModeChat
	rec, err = s.CreateSession(context.Background(), rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.BindSessionAgentAccount(context.Background(), rec.ID, domain.AgentAccountProviderClaude, "device-claude", time.Now()); err != nil {
		t.Fatal(err)
	}
	return s, db, rec
}

func settingsOperation(id domain.SessionID) domain.AgentSettingsOperation {
	return domain.AgentSettingsOperation{ID: "op-1", SessionID: id, IdempotencyKey: "key-1", BaseRevision: 0,
		Source:    domain.AgentSettings{Model: "old-model", Effort: "high", FastMode: domain.AgentFastDefault},
		Target:    domain.AgentSettings{Model: "next-model", Effort: "low", FastMode: domain.AgentFastOff},
		AccountID: "device-claude", ControllerGeneration: "generation-1", Actor: domain.AgentSettingsActor{Kind: domain.AgentSettingsActorLocal},
		CreatedAt: time.Date(2026, 10, 29, 1, 0, 0, 0, time.UTC)}
}

func TestAgentSettingsCommitIsAtomicAndIdempotent(t *testing.T) {
	ctx := context.Background()
	s, db, rec := settingsFixture(t)
	conv, err := s.CreateConversation(ctx, "settings-conv", domain.ConversationScopeSession, rec.ProjectID, rec.ID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	op := settingsOperation(rec.ID)
	accepted, err := s.BeginAgentSettingsOperation(ctx, op)
	if err != nil || accepted.Phase != domain.AgentSettingsPending {
		t.Fatalf("begin: %+v, %v", accepted, err)
	}
	if _, err = s.BeginAgentSettingsOperation(ctx, op); err != nil {
		t.Fatalf("retry begin: %v", err)
	}
	got, err := s.CommitAgentSettingsOperation(ctx, rec.ID, op.ID, time.Now())
	if err != nil || got.Phase != domain.AgentSettingsApplied {
		t.Fatalf("commit: %+v, %v", got, err)
	}
	if _, err = s.CommitAgentSettingsOperation(ctx, rec.ID, op.ID, time.Now()); err != nil {
		t.Fatalf("retry commit: %v", err)
	}
	settings, err := s.SessionAgentSettings(ctx, rec.ID)
	if err != nil || settings.Settings != op.Target || settings.Revision != 1 || settings.AccountID != "device-claude" || settings.OwnerGeneration != "generation-1" {
		t.Fatalf("settings: %+v, %v", settings, err)
	}
	saved, _, _ := s.GetSession(ctx, rec.ID)
	if saved.Metadata.Model != "next-model" || saved.Metadata.Effort != "low" {
		t.Fatalf("metadata: %+v", saved.Metadata)
	}
	var model, effort, fast string
	if err = db.QueryRow("SELECT model,reasoning_effort,agent_fast_mode FROM conversations WHERE id=?", conv.ID).Scan(&model, &effort, &fast); err != nil {
		t.Fatal(err)
	}
	if model != "next-model" || effort != "low" || fast != "off" {
		t.Fatalf("projection: %s/%s/%s", model, effort, fast)
	}
	launch, err := s.SessionLaunchOptions(ctx, rec.ID)
	if err != nil || !launch.FastModeOff || launch.FastMode {
		t.Fatalf("launch: %+v, %v", launch, err)
	}
	var audits int
	if err = db.QueryRow("SELECT COUNT(*) FROM session_agent_settings_audit WHERE operation_id=? AND phase='applied'", op.ID).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("audit count=%d, %v", audits, err)
	}
	op.ID = "retry-with-new-id"
	if retry, err := s.BeginAgentSettingsOperation(ctx, op); err != nil || retry.ID != "op-1" {
		t.Fatalf("idempotent identity: %+v %v", retry, err)
	}
	op.Target.Effort = "max"
	if _, err = s.BeginAgentSettingsOperation(ctx, op); !errors.Is(err, ports.ErrAgentSettingsConflict) {
		t.Fatalf("key reuse: %v", err)
	}
}

func TestAgentSettingsCommitRollsBackProjectionAndAuditFailure(t *testing.T) {
	for _, table := range []string{"conversations", "session_agent_settings_audit"} {
		t.Run(table, func(t *testing.T) {
			ctx := context.Background()
			s, db, rec := settingsFixture(t)
			if _, err := s.CreateConversation(ctx, "settings-conv", domain.ConversationScopeSession, rec.ProjectID, rec.ID, time.Now()); err != nil {
				t.Fatal(err)
			}
			op := settingsOperation(rec.ID)
			if _, err := s.BeginAgentSettingsOperation(ctx, op); err != nil {
				t.Fatal(err)
			}
			event := "UPDATE"
			if table == "session_agent_settings_audit" {
				event = "INSERT"
			}
			if _, err := db.Exec("CREATE TRIGGER settings_fault BEFORE " + event + " ON " + table + " BEGIN SELECT RAISE(ABORT,'injected'); END"); err != nil {
				t.Fatal(err)
			}
			if _, err := s.CommitAgentSettingsOperation(ctx, rec.ID, op.ID, time.Now()); err == nil {
				t.Fatal("injected write succeeded")
			}
			saved, err := s.SessionAgentSettings(ctx, rec.ID)
			if err != nil || saved.Settings != op.Source || saved.Revision != 0 {
				t.Fatalf("partial commit: %+v %v", saved, err)
			}
			var projectedModel, projectedEffort sql.NullString
			var projectedFast string
			if err := db.QueryRow("SELECT model,reasoning_effort,agent_fast_mode FROM conversations WHERE id='settings-conv'").Scan(&projectedModel, &projectedEffort, &projectedFast); err != nil {
				t.Fatal(err)
			}
			if projectedModel.Valid || projectedEffort.Valid || projectedFast != "default" {
				t.Fatalf("partial projection: %v/%v/%s", projectedModel, projectedEffort, projectedFast)
			}
			launch, err := s.SessionLaunchOptions(ctx, rec.ID)
			if err != nil || launch.FastMode || launch.FastModeOff {
				t.Fatalf("partial launch: %+v %v", launch, err)
			}
			var applied int
			if err := db.QueryRow("SELECT COUNT(*) FROM session_agent_settings_audit WHERE phase='applied'").Scan(&applied); err != nil || applied != 0 {
				t.Fatalf("partial audit: %d %v", applied, err)
			}
			pending, err := s.AgentSettingsOperation(ctx, rec.ID, op.ID)
			if err != nil || pending.Phase != domain.AgentSettingsPending {
				t.Fatalf("operation: %+v %v", pending, err)
			}
		})
	}
}

func TestAgentSettingsCommitRejectsStaleOwnerAndConcurrentEdit(t *testing.T) {
	for _, field := range []string{"model", "controller_generation", "is_terminated", "account", "provider_conversation_id", "branch"} {
		t.Run(field, func(t *testing.T) {
			ctx := context.Background()
			s, db, rec := settingsFixture(t)
			op := settingsOperation(rec.ID)
			if _, err := s.BeginAgentSettingsOperation(ctx, op); err != nil {
				t.Fatal(err)
			}
			value := any("external")
			if field == "is_terminated" {
				value = 1
			}
			if field == "branch" {
				if _, err := s.CreateConversation(ctx, "new-branch", domain.ConversationScopeSession, rec.ProjectID, rec.ID, time.Now()); err != nil {
					t.Fatal(err)
				}
			} else if field == "account" {
				if err := s.BindSessionAgentAccount(ctx, rec.ID, domain.AgentAccountProviderClaude, "managed", time.Now()); err != nil {
					t.Fatal(err)
				}
			} else if _, err := db.Exec("UPDATE sessions SET "+field+"=? WHERE id=?", value, rec.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := s.CommitAgentSettingsOperation(ctx, rec.ID, op.ID, time.Now()); !errors.Is(err, ports.ErrAgentSettingsConflict) {
				t.Fatalf("stale commit: %v", err)
			}
		})
	}
}

func TestAgentSettingsLegacyLaunchRemainsReadable(t *testing.T) {
	ctx := context.Background()
	s, _, rec := settingsFixture(t)
	for _, tc := range []struct {
		launch domain.AgentLaunchOptions
		mode   domain.AgentFastMode
	}{
		{domain.AgentLaunchOptions{FastMode: true}, domain.AgentFastOn},
		{domain.AgentLaunchOptions{FastModeOff: true}, domain.AgentFastOff},
		{domain.AgentLaunchOptions{}, domain.AgentFastDefault},
	} {
		if err := s.SetSessionLaunchOptions(ctx, rec.ID, tc.launch); err != nil {
			t.Fatal(err)
		}
		got, err := s.SessionAgentSettings(ctx, rec.ID)
		if err != nil || got.Settings.FastMode != tc.mode {
			t.Fatalf("legacy: %+v %v", got, err)
		}
	}
}

func TestAgentSettingsRejectsConflictingLegacyLaunch(t *testing.T) {
	s, _, rec := settingsFixture(t)
	bad := domain.AgentLaunchOptions{FastMode: true, FastModeOff: true}
	if err := s.SetSessionLaunchOptions(context.Background(), rec.ID, bad); err == nil {
		t.Fatal("conflicting fast choices saved")
	}
	if err := s.UpdateSessionWithLaunchOptions(context.Background(), rec, bad); err == nil {
		t.Fatal("conflicting fast choices saved with session")
	}
}

func TestAgentSettingsConcurrentIntentionsHaveOneWinner(t *testing.T) {
	ctx := context.Background()
	s, _, rec := settingsFixture(t)
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, key := range []string{"a", "b"} {
		go func(key string) {
			<-start
			op := settingsOperation(rec.ID)
			op.ID = key
			op.IdempotencyKey = key
			_, err := s.BeginAgentSettingsOperation(ctx, op)
			results <- err
		}(key)
	}
	close(start)
	wins, conflicts := 0, 0
	for range 2 {
		err := <-results
		if err == nil {
			wins++
		} else if errors.Is(err, ports.ErrAgentSettingsConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("wins=%d conflicts=%d", wins, conflicts)
	}
}

func TestAgentSettingsUnboundAccountCannotClaimDeviceOwner(t *testing.T) {
	ctx := context.Background()
	s, db, rec := settingsFixture(t)
	if _, err := db.Exec("DELETE FROM session_agent_accounts WHERE session_id=?", rec.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAgentAccountDefault(ctx, domain.AgentAccountProviderClaude, "managed-claude"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginAgentSettingsOperation(ctx, settingsOperation(rec.ID)); !errors.Is(err, ports.ErrAgentSettingsConflict) {
		t.Fatalf("unbound session accepted device owner: %v", err)
	}
	got, err := s.SessionAgentSettings(ctx, rec.ID)
	if err != nil || got.AccountID != "" || got.Revision != 0 {
		t.Fatalf("invented bound account: %+v, %v", got, err)
	}
}
