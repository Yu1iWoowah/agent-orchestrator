package agentsettings

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

func TestReadSettingsCapabilitiesDoNotCrossAccountOrTrustStaleCatalog(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	s := sqlitetest.MustOpen(t)
	if err := s.UpsertProject(ctx, domain.ProjectRecord{ID: "settings", Path: "/fake", RegisteredAt: now}); err != nil {
		t.Fatal(err)
	}
	rec, err := s.CreateSession(ctx, domain.SessionRecord{ProjectID: "settings", Kind: domain.KindWorker, Harness: domain.HarnessCodex, Mode: domain.SessionModeChat, Metadata: domain.SessionMetadata{Model: "native-model", ControllerGeneration: "generation-1"}, CreatedAt: now, UpdatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	catalog := ports.AgentModelCatalog{AgentID: "codex", Source: "codex-app-server", SelectionMode: ports.ModelSelectionCatalog, Models: []ports.AgentModelInfo{{ID: "native-model", Efforts: []string{"low", "high"}, SupportsFast: true}, {ID: "small", SupportsFast: false}}}
	raw, err := json.Marshal(catalog)
	if err != nil {
		t.Fatal(err)
	}
	cached := ports.CachedAgentModelCatalog{AgentID: "codex", CatalogJSON: string(raw), Source: "codex-app-server", FetchedAt: now, LastSuccessAt: now, Generation: 7}
	if err = s.UpsertAgentModelCatalog(ctx, cached); err != nil {
		t.Fatal(err)
	}
	svc := New(s, func() time.Time { return now })
	got, err := svc.Read(ctx, rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Capabilities.State != CapabilitySupported || got.Capabilities.AccountID != "device-codex" || got.Capabilities.Generation != 7 || len(got.Capabilities.Models) != 2 || got.Capabilities.Models[0].Fast != CapabilitySupported || got.Capabilities.Models[1].Fast != CapabilityUnknown {
		t.Fatalf("fresh: %+v", got.Capabilities)
	}
	cached.LastSuccessAt = now.Add(-7 * time.Hour)
	cached.FetchedAt = cached.LastSuccessAt
	if err = s.UpsertAgentModelCatalog(ctx, cached); err != nil {
		t.Fatal(err)
	}
	got, err = svc.Read(ctx, rec.ID)
	if err != nil || got.Capabilities.State != CapabilityUnknown || len(got.Capabilities.Models) != 0 {
		t.Fatalf("stale: %+v %v", got, err)
	}
	if err = s.BindSessionAgentAccount(ctx, rec.ID, domain.AgentAccountProviderCodex, "managed-codex", now); err != nil {
		t.Fatal(err)
	}
	got, err = svc.Read(ctx, rec.ID)
	if err != nil || got.Capabilities.State != CapabilityUnknown || got.Capabilities.AccountID != "managed-codex" || got.Capabilities.Reason != "account_catalog_unavailable" {
		t.Fatalf("account: %+v %v", got, err)
	}
}

func TestReadSettingsReturnsNotFound(t *testing.T) {
	svc := New(sqlitetest.MustOpen(t), time.Now)
	if _, err := svc.Read(context.Background(), "missing"); !errors.Is(err, ports.ErrAgentSettingsNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func TestReadSettingsDoesNotPromoteInvalidOrModeCatalog(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	s := sqlitetest.MustOpen(t)
	if err := s.UpsertProject(ctx, domain.ProjectRecord{ID: "catalog", Path: "/fake", RegisteredAt: now}); err != nil {
		t.Fatal(err)
	}
	rec, err := s.CreateSession(ctx, domain.SessionRecord{ProjectID: "catalog", Kind: domain.KindWorker, Harness: domain.HarnessCodex, CreatedAt: now, UpdatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, raw, reason string }{
		{"invalid JSON", "{", "catalog_invalid"},
		{"wrong harness", `{"agentId":"claude-code","models":[{"id":"opus","supportsFast":true}]}`, "catalog_scope_mismatch"},
		{"modes are not models", `{"agentId":"codex","selectionMode":"mode","models":[{"id":"high","supportsFast":true}]}`, "model_settings_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := s.UpsertAgentModelCatalog(ctx, ports.CachedAgentModelCatalog{AgentID: "codex", CatalogJSON: tc.raw, Source: "provider", FetchedAt: now, LastSuccessAt: now}); err != nil {
				t.Fatal(err)
			}
			got, err := New(s, func() time.Time { return now }).Read(ctx, rec.ID)
			if err != nil || got.Capabilities.State != CapabilityUnknown || got.Capabilities.Reason != tc.reason || len(got.Capabilities.Models) != 0 {
				t.Fatalf("got %+v %v", got, err)
			}
		})
	}
}
