package store_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestPreparedHarnessSelectionPreservesWorkspaceAndVisibility(t *testing.T) {
	ctx := context.Background()
	st, db, rec := settingsFixture(t)
	if changed, err := st.SetTaskPreparationHarness(ctx, rec.ID, domain.HarnessCodex); err != nil || changed {
		t.Fatalf("visible session changed: changed=%v err=%v", changed, err)
	}
	if _, err := db.Exec("UPDATE sessions SET is_task_preparation=1,harness='',branch='prepared',workspace_path='/prepared',workspace_repo_path='/repo' WHERE id=?", rec.ID); err != nil {
		t.Fatal(err)
	}
	before, _, err := st.GetSession(ctx, rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := st.SetTaskPreparationHarness(ctx, rec.ID, domain.HarnessCodex); err != nil || !changed {
		t.Fatalf("select harness: changed=%v err=%v", changed, err)
	}
	after, _, err := st.GetSession(ctx, rec.ID)
	before.Harness = domain.HarnessCodex
	before.Revision++ // Existing AFTER UPDATE trigger owns session revision.
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("harness selection changed workspace/visibility facts: before=%+v after=%+v err=%v", before, after, err)
	}
	if changed, err := st.SetTaskPreparationHarness(ctx, rec.ID, domain.HarnessClaudeCode); err != nil || changed {
		t.Fatalf("selected harness overwritten: changed=%v err=%v", changed, err)
	}
}

func TestSpawnAgentSettingsPinsSelectionAndSources(t *testing.T) {
	ctx := context.Background()
	st, db, rec := settingsFixture(t)
	saver, ok := any(st).(interface {
		SaveSpawnAgentSettings(context.Context, domain.SessionID, domain.SpawnAgentSettingsResolution) error
	})
	if !ok {
		t.Fatal("durable spawn settings are unavailable")
	}
	resolution := domain.SpawnAgentSettingsResolution{Settings: domain.AgentSettings{FastMode: domain.AgentFastOff}, Sources: domain.AgentSettingsSources{Model: "request", Effort: "request", FastMode: "role"}, Validation: "catalog", AccountID: "device-claude"}
	if err := saver.SaveSpawnAgentSettings(ctx, rec.ID, resolution); err != nil {
		t.Fatal(err)
	}
	got, found, err := st.GetSession(ctx, rec.ID)
	if err != nil || !found || got.Metadata.Model != "" || got.Metadata.Effort != "" || got.Metadata.FastMode != domain.AgentFastOff || !reflect.DeepEqual(got.Metadata.SpawnAgentSettings, &resolution) {
		t.Fatalf("read=%+v found=%v err=%v", got.Metadata, found, err)
	}
	if err = saver.SaveSpawnAgentSettings(ctx, rec.ID, resolution); err != nil {
		t.Fatalf("same decision retry: %v", err)
	}
	changed := resolution
	changed.Settings.FastMode = domain.AgentFastOn
	if err = saver.SaveSpawnAgentSettings(ctx, rec.ID, changed); !errors.Is(err, ports.ErrAgentSettingsConflict) {
		t.Fatalf("rewrote immutable decision: %v", err)
	}
	// A different seed must not accept catalog evidence for its former account.
	if _, err = db.Exec("UPDATE sessions SET spawn_agent_settings_json='' WHERE id=?", rec.ID); err != nil {
		t.Fatal(err)
	}
	if err = st.BindSessionAgentAccount(ctx, rec.ID, domain.AgentAccountProviderClaude, "managed", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = saver.SaveSpawnAgentSettings(ctx, rec.ID, changed); !errors.Is(err, ports.ErrAgentSettingsConflict) {
		t.Fatalf("cross-account decision accepted: %v", err)
	}
	settings, err := st.SessionAgentSettings(ctx, rec.ID)
	if err != nil || settings.Settings.FastMode != domain.AgentFastOff {
		t.Fatalf("failed CAS changed preferences: %+v err=%v", settings, err)
	}
}

func TestSpawnAgentSettingsStatementFailureLeavesAllFieldsUnchanged(t *testing.T) {
	ctx := context.Background()
	st, db, rec := settingsFixture(t)
	before, _, err := st.GetSession(ctx, rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`CREATE TRIGGER reject_spawn_settings BEFORE UPDATE OF spawn_agent_settings_json ON sessions BEGIN SELECT RAISE(ABORT,'injected failure'); END`); err != nil {
		t.Fatal(err)
	}
	resolution := domain.SpawnAgentSettingsResolution{Settings: domain.AgentSettings{Model: "next", FastMode: domain.AgentFastOn}, AccountID: "device-claude", Validation: "catalog"}
	if err = st.SaveSpawnAgentSettings(ctx, rec.ID, resolution); err == nil {
		t.Fatal("expected injected write failure")
	}
	after, _, err := st.GetSession(ctx, rec.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("partial spawn decision persisted: before=%+v after=%+v err=%v", before, after, err)
	}
}
