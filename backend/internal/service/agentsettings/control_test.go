package agentsettings

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

type durableOnlyRuntime struct{ st *sqlite.Store }

func (r durableOnlyRuntime) QueueAgentSettings(ctx context.Context, op domain.AgentSettingsOperation) (domain.AgentSettingsOperation, error) {
	return r.st.BeginAgentSettingsOperation(ctx, op)
}

func controlFixture(t *testing.T) (*Control, *sqlite.Store, domain.SessionRecord) {
	t.Helper()
	ctx := t.Context()
	now := time.Now()
	st := sqlitetest.MustOpen(t)
	if err := st.UpsertProject(ctx, domain.ProjectRecord{ID: "p", Path: "/fake", RegisteredAt: now}); err != nil {
		t.Fatal(err)
	}
	rec, err := st.CreateSession(ctx, domain.SessionRecord{ProjectID: "p", Kind: domain.KindWorker, Harness: domain.HarnessCodex, Mode: domain.SessionModeChat, Metadata: domain.SessionMetadata{Model: "native", ControllerGeneration: "g", ProviderConversationID: "native-thread"}, CreatedAt: now, UpdatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.BindSessionAgentAccount(ctx, rec.ID, domain.AgentAccountProviderCodex, "device-codex", now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateConversation(ctx, "conversation", domain.ConversationScopeSession, "p", rec.ID, now); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"agentId": "codex", "models": []map[string]any{{"id": "native", "isDefault": true, "efforts": []string{"low", "high"}, "supportsFast": true}}})
	if err := st.UpsertAgentModelCatalog(ctx, ports.CachedAgentModelCatalog{AgentID: "codex", CatalogJSON: string(raw), LastSuccessAt: now, FetchedAt: now, InputFingerprint: "current"}); err != nil {
		t.Fatal(err)
	}
	core := New(st, func() time.Time { return now })
	core.SetCatalogFingerprintSource(fixedCatalogFingerprint("current"))
	return NewControl(core, st, NewAuthorizer(st, nil, func() time.Time { return now }), durableOnlyRuntime{st}, func() time.Time { return now }), st, rec
}

func TestSettingsControlCASIdempotencyAndCancel(t *testing.T) {
	c, st, rec := controlFixture(t)
	ctx := t.Context()
	zero := int64(0)
	high := domain.AgentEffortSelection("high")
	in := Change{ExpectedRevision: &zero, IdempotencyKey: "first", Settings: domain.AgentSettingsPatch{Effort: &high}}
	op, err := c.Change(ctx, Caller{}, rec.ID, in)
	if err != nil || op.Phase != domain.AgentSettingsPending || op.Actor.Kind != domain.AgentSettingsActorLocal {
		t.Fatalf("accepted: %+v %v", op, err)
	}
	in.RequestID = "new-request-id"
	retry, err := c.Change(ctx, Caller{}, rec.ID, in)
	if err != nil || retry.ID != op.ID {
		t.Fatalf("retry changed accepted intent: %+v %v", retry, err)
	}
	if _, err := st.CommitAgentSettingsOperation(ctx, rec.ID, op.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	retry, err = c.Change(ctx, Caller{}, rec.ID, in)
	if err != nil || retry.ID != op.ID || retry.Phase != domain.AgentSettingsApplied {
		t.Fatalf("retry reinterpreted current revision: %+v %v", retry, err)
	}
	low := domain.AgentEffortSelection("low")
	in.Settings.Effort = &low
	if _, err = c.Change(ctx, Caller{}, rec.ID, in); !hasCode(err, "AGENT_SETTINGS_CONFLICT") {
		t.Fatalf("key reuse: %v", err)
	}
	if _, err = c.Change(ctx, Caller{}, rec.ID, Change{IdempotencyKey: "missing", Settings: in.Settings}); !hasCode(err, "AGENT_SETTINGS_INVALID") {
		t.Fatalf("missing CAS accepted: %v", err)
	}
	one := int64(1)
	in.ExpectedRevision = &one
	in.IdempotencyKey = "cancel"
	op, err = c.Change(ctx, Caller{}, rec.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Cancel(ctx, Caller{}, rec.ID, op.ID, op.Version)
	if err != nil || got.Phase != domain.AgentSettingsCancelled {
		t.Fatalf("cancel %+v %v", got, err)
	}
	in.IdempotencyKey = "next"
	if _, err := c.Change(ctx, Caller{}, rec.ID, in); err != nil {
		t.Fatal(err)
	}
}

type racedSettingsLookup struct {
	ControlStore
	once   sync.Once
	accept func()
}

func (s *racedSettingsLookup) AgentSettingsOperationByKey(ctx context.Context, id domain.SessionID, key string) (domain.AgentSettingsOperation, bool, error) {
	raced := false
	s.once.Do(func() { raced = true; s.accept() })
	if raced {
		return domain.AgentSettingsOperation{}, false, nil
	}
	return s.ControlStore.AgentSettingsOperationByKey(ctx, id, key)
}

func TestSettingsControlConcurrentDuplicateReturnsOriginalCommit(t *testing.T) {
	base, st, rec := controlFixture(t)
	revision := int64(0)
	high := domain.AgentEffortSelection("high")
	in := Change{ExpectedRevision: &revision, IdempotencyKey: "same", Settings: domain.AgentSettingsPatch{Effort: &high}}
	var original domain.AgentSettingsOperation
	store := &racedSettingsLookup{ControlStore: st, accept: func() {
		var err error
		original, err = base.Change(t.Context(), Caller{}, rec.ID, in)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.CommitAgentSettingsOperation(t.Context(), rec.ID, original.ID, time.Now()); err != nil {
			t.Fatal(err)
		}
	}}
	racing := NewControl(base.core, store, base.authorizer, base.runtime, base.now)
	got, err := racing.Change(t.Context(), Caller{}, rec.ID, in)
	if err != nil || got.ID != original.ID || got.Phase != domain.AgentSettingsApplied {
		t.Fatalf("concurrent duplicate reinterpreted revision: %+v %v", got, err)
	}
}

func TestSettingsControlConcurrentClientsHaveOneRevisionWinner(t *testing.T) {
	c, _, rec := controlFixture(t)
	rev := int64(0)
	effort := domain.AgentEffortSelection("high")
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, key := range []string{"cli", "ui"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := c.Change(t.Context(), Caller{}, rec.ID, Change{ExpectedRevision: &rev, IdempotencyKey: key, Settings: domain.AgentSettingsPatch{Effort: &effort}})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	winners := 0
	for err := range errs {
		if err == nil {
			winners++
		} else if !hasCode(err, "AGENT_SETTINGS_CONFLICT") {
			t.Fatal(err)
		}
	}
	if winners != 1 {
		t.Fatalf("winners=%d", winners)
	}
}

func TestSettingsControlModelChangeOnlyClearsIncompatibleInheritedEffort(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "inherited", true: "explicit"}[explicit], func(t *testing.T) {
			c, st, rec := controlFixture(t)
			rec.Metadata.Effort = "high"
			if err := st.UpdateSession(t.Context(), rec); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(ports.AgentModelCatalog{AgentID: "codex", Models: []ports.AgentModelInfo{{ID: "narrow", IsDefault: true, Efforts: []string{"low"}, SupportsFast: true}}})
			if err := st.UpsertAgentModelCatalog(t.Context(), ports.CachedAgentModelCatalog{AgentID: "codex", CatalogJSON: string(raw), InputFingerprint: "current", LastSuccessAt: c.now(), FetchedAt: c.now()}); err != nil {
				t.Fatal(err)
			}
			current, err := st.SessionAgentSettings(t.Context(), rec.ID)
			if err != nil {
				t.Fatal(err)
			}
			revision := current.Revision
			model := domain.AgentModelSelection("narrow")
			patch := domain.AgentSettingsPatch{Model: &model}
			if explicit {
				effort := domain.AgentEffortSelection("high")
				patch.Effort = &effort
			}
			op, err := c.Change(t.Context(), Caller{}, rec.ID, Change{ExpectedRevision: &revision, Settings: patch, IdempotencyKey: "model"})
			if explicit {
				if !hasCode(err, "AGENT_SETTINGS_UNSUPPORTED") {
					t.Fatalf("explicit effort: %+v %v", op, err)
				}
				return
			}
			if err != nil || op.Target.Model != "narrow" || op.Target.Effort != "" {
				t.Fatalf("inherited effort not reset: %+v %v", op, err)
			}
		})
	}
}

func hasCode(err error, code string) bool {
	var e *apierr.Error
	return errors.As(err, &e) && e.Code == code
}
