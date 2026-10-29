package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

type turnSettingsRecorder interface {
	RecordTurnSettingsSnapshot(context.Context, string, string, []string, domain.AgentSettingsTurnSnapshot) error
}

func dispatchSnapshot(t *testing.T, settings domain.SessionAgentSettings) domain.AgentSettingsTurnSnapshot {
	t.Helper()
	at := time.Now().UTC()
	raw, err := json.Marshal(map[string]any{
		"state": "known", "settings": settings.Settings, "revision": settings.Revision, "capturedAt": at, "source": "root",
		"owner": domain.AgentSettingsOwner{SessionID: settings.SessionID, AccountID: settings.AccountID, ControllerGeneration: settings.OwnerGeneration, ProviderConversationID: settings.ProviderConversationID, BranchID: settings.BranchID},
	})
	if err != nil {
		t.Fatal(err)
	}
	var result domain.AgentSettingsTurnSnapshot
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestTurnSettingsSnapshotSurvivesReloadWithoutRewritingHistory(t *testing.T) {
	ctx := context.Background()
	s, db, rec := settingsStartFixture(t, "")
	writer, ok := any(s).(turnSettingsRecorder)
	if !ok {
		t.Fatal("turn dispatch settings have no durable snapshot writer")
	}
	current, err := s.SessionAgentSettings(ctx, rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"legacy", "captured"} {
		if _, err := s.AppendUserMessage(ctx, "start-context", rec.ID, current.OwnerGeneration, domain.ConversationMessage{ID: "message-" + id, Text: "unchanged " + id, Origin: domain.MessageOriginHuman}, id, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	frame := dispatchSnapshot(t, current)
	var beforeEvents int
	if err := db.QueryRow("SELECT COUNT(*) FROM change_log WHERE session_id=?", rec.ID).Scan(&beforeEvents); err != nil {
		t.Fatal(err)
	}
	if err := writer.RecordTurnSettingsSnapshot(ctx, "start-context", current.OwnerGeneration, []string{"captured"}, frame); err != nil {
		t.Fatal(err)
	}
	if err := writer.RecordTurnSettingsSnapshot(ctx, "start-context", current.OwnerGeneration, []string{"captured"}, frame); err != nil {
		t.Fatalf("same immutable receipt retry: %v", err)
	}
	var afterEvents int
	if err := db.QueryRow("SELECT COUNT(*) FROM change_log WHERE session_id=?", rec.ID).Scan(&afterEvents); err != nil || afterEvents != beforeEvents+1 {
		t.Fatalf("snapshot CDC duplicated or missing: %d -> %d, %v", beforeEvents, afterEvents, err)
	}
	if _, err := db.Exec("UPDATE sessions SET model='later-model',agent_settings_revision=agent_settings_revision+1 WHERE id=?", rec.ID); err != nil {
		t.Fatal(err)
	}
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
	for _, id := range []string{"legacy", "captured"} {
		turn, err := reopened.TurnByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(turn)
		var wire map[string]any
		if err := json.Unmarshal(raw, &wire); err != nil {
			t.Fatal(err)
		}
		if id == "legacy" {
			if wire["agentSettings"] != nil {
				t.Fatalf("fabricated historical settings: %s", raw)
			}
			continue
		}
		got, ok := wire["agentSettings"].(map[string]any)
		if !ok || got["revision"] != float64(0) || got["source"] != "root" || got["turnId"] != id {
			t.Fatalf("dispatch snapshot lost on reopen: %s", raw)
		}
		selection, ok := got["settings"].(map[string]any)
		if !ok || selection["model"] != "old-model" {
			t.Fatalf("history borrowed current preferences: %s", raw)
		}
	}
}

func TestTurnSettingsSnapshotFencesAndBatchRollback(t *testing.T) {
	ctx := context.Background()
	s, db, rec := settingsStartFixture(t, "")
	writer, ok := any(s).(turnSettingsRecorder)
	if !ok {
		t.Fatal("turn dispatch settings have no durable snapshot writer")
	}
	current, err := s.SessionAgentSettings(ctx, rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"first", "second"} {
		if _, err := s.AppendUserMessage(ctx, "start-context", rec.ID, current.OwnerGeneration, domain.ConversationMessage{ID: "message-" + id, Text: id, Origin: domain.MessageOriginHuman}, id, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	frame := dispatchSnapshot(t, current)
	if err := writer.RecordTurnSettingsSnapshot(ctx, "start-context", "obsolete", []string{"first"}, frame); !errors.Is(err, ports.ErrAgentSettingsConflict) {
		t.Fatalf("stale generation accepted: %v", err)
	}
	staleRevision := current.Revision + 1
	stale := frame
	stale.Revision = &staleRevision
	if err := writer.RecordTurnSettingsSnapshot(ctx, "start-context", current.OwnerGeneration, []string{"first"}, stale); !errors.Is(err, ports.ErrAgentSettingsConflict) {
		t.Fatalf("borrowed another revision: %v", err)
	}
	if _, err := db.Exec("CREATE TRIGGER snapshot_fault BEFORE UPDATE OF agent_settings_snapshot_json ON conversation_turns WHEN NEW.id='second' BEGIN SELECT RAISE(ABORT,'snapshot-injected'); END"); err != nil {
		t.Fatal(err)
	}
	if err := writer.RecordTurnSettingsSnapshot(ctx, "start-context", current.OwnerGeneration, []string{"first", "second"}, frame); err == nil {
		t.Fatal("batch snapshot fault ignored")
	}
	var written int
	if err := db.QueryRow("SELECT COUNT(*) FROM conversation_turns WHERE agent_settings_snapshot_json<>''").Scan(&written); err != nil || written != 0 {
		t.Fatalf("partial batch snapshot: %d %v", written, err)
	}
	if _, err := db.Exec("DROP TRIGGER snapshot_fault"); err != nil {
		t.Fatal(err)
	}
	if err := writer.RecordTurnSettingsSnapshot(ctx, "start-context", current.OwnerGeneration, []string{"first", "second"}, frame); err != nil {
		t.Fatal(err)
	}
	changed := frame
	selection := current.Settings
	selection.Effort = "low"
	changed.Settings, changed.Revision = &selection, nil
	if err := writer.RecordTurnSettingsSnapshot(ctx, "start-context", current.OwnerGeneration, []string{"first"}, changed); !errors.Is(err, ports.ErrAgentSettingsConflict) {
		t.Fatalf("immutable wire snapshot overwritten: %v", err)
	}
}

func TestTurnSettingsSnapshotAttributesClaimedReceiptToCurrentDispatcher(t *testing.T) {
	ctx := context.Background()
	s, db, rec := settingsStartFixture(t, "")
	current, err := s.SessionAgentSettings(ctx, rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	previous, err := s.CreateSession(ctx, domain.SessionRecord{ProjectID: rec.ProjectID, Kind: domain.KindWorker, Harness: domain.HarnessClaudeCode, Mode: domain.SessionModeChat, Activity: domain.Activity{State: domain.ActivityIdle, LastActivityAt: now}, CreatedAt: now, UpdatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendUserMessage(ctx, "start-context", rec.ID, current.OwnerGeneration, domain.ConversationMessage{ID: "carried-message", Text: "preserve the receipt", Origin: domain.MessageOriginHuman}, "carried", now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO conversation_branches(id,conversation_id,session_id,provider_conversation_id,created_at) VALUES('previous-branch','start-context',?,'previous-native',?)", previous.ID, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE conversation_branches SET parent_branch_id='previous-branch' WHERE id=?", current.BranchID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE conversation_turns SET handled_by_session_id=?,controller_generation='previous-generation',branch_id='previous-branch' WHERE id='carried'", previous.ID); err != nil {
		t.Fatal(err)
	}
	if batch, err := s.ClaimQueuedBatch(ctx, "start-context", true, 0, now); err != nil || len(batch) != 1 {
		t.Fatalf("claim existing receipt: %+v %v", batch, err)
	}
	if err := s.RecordTurnSettingsSnapshot(ctx, "start-context", current.OwnerGeneration, []string{"carried"}, dispatchSnapshot(t, current)); err != nil {
		t.Fatalf("claimed receipt lost its current dispatcher: %v", err)
	}
	turn, err := s.TurnByID(ctx, "carried")
	if err != nil || turn.HandledBySessionID != previous.ID || turn.BranchID != "previous-branch" || turn.AgentSettings == nil || turn.AgentSettings.Owner == nil || turn.AgentSettings.Owner.SessionID != rec.ID || turn.AgentSettings.Owner.BranchID != current.BranchID {
		t.Fatalf("receipt history or dispatch attribution changed: %+v %v", turn, err)
	}
	if _, err := s.AppendUserMessage(ctx, "start-context", rec.ID, current.OwnerGeneration, domain.ConversationMessage{ID: "historical-message", Text: "old completed request", Origin: domain.MessageOriginHuman}, "completed", now); err != nil {
		t.Fatal(err)
	}
	if err := s.SettleTurnByID(ctx, "completed", domain.TurnStateCompleted, "", now); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordTurnSettingsSnapshot(ctx, "start-context", current.OwnerGeneration, []string{"completed"}, dispatchSnapshot(t, current)); !errors.Is(err, ports.ErrAgentSettingsConflict) {
		t.Fatalf("completed history was backfilled: %v", err)
	}
}
