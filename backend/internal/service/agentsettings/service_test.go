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
	if err := s.BindSessionAgentAccount(ctx, rec.ID, domain.AgentAccountProviderCodex, "device-codex", now); err != nil {
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
	cached.LastSuccessAt = now.AddDate(0, 0, -1)
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
	if err := s.BindSessionAgentAccount(ctx, rec.ID, domain.AgentAccountProviderCodex, "device-codex", now); err != nil {
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

func TestReadSettingsRequiresValidSuccessfulCatalog(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	for _, tc := range []struct {
		name, state, catalogState, refreshError, catalogError, reason string
		lastSuccess                                                   time.Time
		stale                                                         bool
	}{
		{name: "invalidated", state: "idle", reason: "catalog_unverified"},
		{name: "queued invalidation", state: "queued", reason: "catalog_stale"},
		{name: "refreshing invalidation", state: "refreshing", reason: "catalog_stale"},
		{name: "never successful", state: "idle", reason: "catalog_unverified"},
		{name: "queued with prior success", state: "queued", lastSuccess: now, reason: "catalog_stale"},
		{name: "refreshing with prior success", state: "refreshing", lastSuccess: now, reason: "catalog_stale"},
		{name: "previous day success", state: "idle", lastSuccess: now.AddDate(0, 0, -1), reason: "catalog_stale"},
		{name: "future success", state: "idle", lastSuccess: now.Add(time.Second), reason: "catalog_stale"},
		{name: "stale catalog", state: "idle", lastSuccess: now, stale: true, reason: "catalog_stale"},
		{name: "refresh failed", state: "error", lastSuccess: now, reason: "catalog_stale"},
		{name: "refresh error", state: "idle", refreshError: "offline", lastSuccess: now, reason: "catalog_stale"},
		{name: "catalog queued", state: "idle", catalogState: "queued", lastSuccess: now, reason: "catalog_stale"},
		{name: "catalog refreshing", state: "idle", catalogState: "refreshing", lastSuccess: now, reason: "catalog_stale"},
		{name: "catalog error", state: "idle", catalogError: "offline", lastSuccess: now, reason: "catalog_stale"},
		{name: "unrecognized catalog state", state: "idle", catalogState: "unrecognized", lastSuccess: now, reason: "catalog_stale"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := sqlitetest.MustOpen(t)
			if err := s.UpsertProject(ctx, domain.ProjectRecord{ID: "catalog", Path: "/fake", RegisteredAt: now}); err != nil {
				t.Fatal(err)
			}
			rec, err := s.CreateSession(ctx, domain.SessionRecord{ProjectID: "catalog", Kind: domain.KindWorker, Harness: domain.HarnessCodex, CreatedAt: now, UpdatedAt: now})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.BindSessionAgentAccount(ctx, rec.ID, domain.AgentAccountProviderCodex, "device-codex", now); err != nil {
				t.Fatal(err)
			}
			catalog := ports.AgentModelCatalog{AgentID: "codex", SelectionMode: ports.ModelSelectionCatalog, Stale: tc.stale, RefreshState: tc.catalogState, RefreshError: tc.catalogError, Models: []ports.AgentModelInfo{{ID: "model", SupportsFast: true}}, LastSuccessAt: &now}
			raw, err := json.Marshal(catalog)
			if err != nil {
				t.Fatal(err)
			}
			// Explicit metadata avoids the store's legacy successful-record normalization.
			if err := s.UpsertAgentModelCatalog(ctx, ports.CachedAgentModelCatalog{AgentID: "codex", CatalogJSON: string(raw), FetchedAt: now, LastSuccessAt: tc.lastSuccess, RefreshState: tc.state, RefreshError: tc.refreshError, Generation: 7}); err != nil {
				t.Fatal(err)
			}
			got, err := New(s, func() time.Time { return now }).Read(ctx, rec.ID)
			if err != nil || got.Capabilities.State != CapabilityUnknown || got.Capabilities.Reason != tc.reason || len(got.Capabilities.Models) != 0 {
				t.Fatalf("unverified evidence promoted: %+v, %v", got.Capabilities, err)
			}
			if tc.lastSuccess.IsZero() && got.Capabilities.ObservedAt != nil {
				t.Fatalf("fetch time presented as successful observation: %v", got.Capabilities.ObservedAt)
			}
		})
	}
}

type catalogEvidenceStore struct {
	settings domain.SessionAgentSettings
	catalog  ports.CachedAgentModelCatalog
}

func (s catalogEvidenceStore) SessionAgentSettings(context.Context, domain.SessionID) (domain.SessionAgentSettings, error) {
	return s.settings, nil
}

func (s catalogEvidenceStore) GetAgentModelCatalog(context.Context, string, string) (ports.CachedAgentModelCatalog, bool, error) {
	return s.catalog, true, nil
}

func TestReadSettingsDoesNotTrustUnknownHarnessCatalog(t *testing.T) {
	now := time.Now().UTC()
	for _, harness := range []domain.AgentHarness{"unknown-harness", domain.HarnessFake, ""} {
		t.Run(string(harness), func(t *testing.T) {
			raw, err := json.Marshal(ports.AgentModelCatalog{AgentID: string(harness), Models: []ports.AgentModelInfo{{ID: "synthetic", SupportsFast: true}}})
			if err != nil {
				t.Fatal(err)
			}
			store := catalogEvidenceStore{
				settings: domain.SessionAgentSettings{SessionID: "unknown", Harness: harness},
				catalog:  ports.CachedAgentModelCatalog{AgentID: string(harness), CatalogJSON: string(raw), FetchedAt: now, LastSuccessAt: now, RefreshState: "idle", Generation: 7},
			}
			got, err := New(store, func() time.Time { return now }).Read(context.Background(), "unknown")
			if err != nil || got.Capabilities.State != CapabilityUnknown || got.Capabilities.Reason != "harness_unknown" || len(got.Capabilities.Models) != 0 {
				t.Fatalf("unknown harness evidence promoted: %+v, %v", got.Capabilities, err)
			}
		})
	}
}

func TestReadSettingsCatalogFreshnessUsesLocalCalendarDay(t *testing.T) {
	local := time.FixedZone("UTC+14", 14*60*60)
	for _, tc := range []struct {
		name             string
		now, lastSuccess time.Time
		want             CapabilityState
	}{
		{"same day over six hours", time.Date(2026, 10, 30, 20, 0, 0, 0, local), time.Date(2026, 10, 30, 9, 0, 0, 0, local), CapabilitySupported},
		{"previous day", time.Date(2026, 10, 30, 20, 0, 0, 0, local), time.Date(2026, 10, 29, 20, 0, 0, 0, local), CapabilityUnknown},
		{"before midnight", time.Date(2026, 10, 30, 23, 59, 0, 0, local), time.Date(2026, 10, 30, 0, 1, 0, 0, local), CapabilitySupported},
		{"after midnight", time.Date(2026, 10, 30, 0, 1, 0, 0, local), time.Date(2026, 10, 29, 23, 59, 0, 0, local), CapabilityUnknown},
		{"future same day", time.Date(2026, 10, 30, 9, 0, 0, 0, local), time.Date(2026, 10, 30, 9, 1, 0, 0, local), CapabilityUnknown},
		{"UTC timestamp same local day", time.Date(2026, 10, 30, 9, 0, 0, 0, local), time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC), CapabilitySupported},
		{"UTC day matches but local day differs", time.Date(2026, 10, 30, 0, 1, 0, 0, local), time.Date(2026, 10, 29, 9, 59, 0, 0, time.UTC), CapabilityUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := catalogEvidenceStore{
				settings: domain.SessionAgentSettings{SessionID: "worker", Harness: domain.HarnessCodex, AccountID: "device-codex"},
				catalog: ports.CachedAgentModelCatalog{
					AgentID: "codex", CatalogJSON: `{"agentId":"codex","models":[{"id":"model","supportsFast":true}]}`,
					LastSuccessAt: tc.lastSuccess.UTC(), FetchedAt: tc.lastSuccess.UTC(), RefreshState: "idle", Generation: 7,
				},
			}
			got, err := New(store, func() time.Time { return tc.now }).Read(context.Background(), "worker")
			if err != nil || got.Capabilities.State != tc.want {
				t.Fatalf("calendar-day evidence: %+v, %v; want %s", got.Capabilities, err, tc.want)
			}
			if tc.want == CapabilitySupported {
				if got.Capabilities.Reason != "" || len(got.Capabilities.Models) != 1 || got.Capabilities.Models[0].Fast != CapabilitySupported {
					t.Fatalf("valid same-day evidence lost: %+v", got.Capabilities)
				}
			} else if got.Capabilities.Reason != "catalog_stale" || len(got.Capabilities.Models) != 0 {
				t.Fatalf("untrusted evidence exposed: %+v", got.Capabilities)
			}
		})
	}
}

func TestReadSettingsUnboundAccountDoesNotUseDeviceCatalog(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	s := sqlitetest.MustOpen(t)
	if err := s.UpsertProject(ctx, domain.ProjectRecord{ID: "unbound", Path: "/fake", RegisteredAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateAgentAccount(ctx, domain.AgentAccount{ID: "managed-codex", Provider: domain.AgentAccountProviderCodex, Home: "/fake/managed", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAgentAccountDefault(ctx, domain.AgentAccountProviderCodex, "managed-codex"); err != nil {
		t.Fatal(err)
	}
	rec, err := s.CreateSession(ctx, domain.SessionRecord{ProjectID: "unbound", Kind: domain.KindWorker, Harness: domain.HarnessCodex, CreatedAt: now, UpdatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertAgentModelCatalog(ctx, ports.CachedAgentModelCatalog{AgentID: "codex", CatalogJSON: `{"agentId":"codex","models":[{"id":"device-model","supportsFast":true}]}`, FetchedAt: now, LastSuccessAt: now, RefreshState: "idle", Generation: 7}); err != nil {
		t.Fatal(err)
	}
	got, err := New(s, func() time.Time { return now }).Read(ctx, rec.ID)
	if err != nil || got.Capabilities.State != CapabilityUnknown || got.Capabilities.Reason != "account_unbound" || got.Capabilities.AccountID != "" || len(got.Capabilities.Models) != 0 {
		t.Fatalf("unbound account promoted device evidence: %+v, %v", got.Capabilities, err)
	}
}
