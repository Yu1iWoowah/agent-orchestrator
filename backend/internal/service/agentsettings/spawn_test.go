package agentsettings

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

func TestSpawnSettingsPreflightUsesBoundAccountEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, account, model, effort, fast, wantReason, wantEffort string
		stale, clearEffort                                         bool
	}{
		{name: "supported", account: "device-codex", model: "native", effort: "high", fast: "on", wantEffort: "high"},
		{name: "provider model default", account: "device-codex", effort: "high", fast: "on", wantEffort: "high"},
		{name: "standard tier", account: "device-codex", model: "native", fast: "off"},
		{name: "managed account", account: "managed", model: "native", fast: "on", wantReason: "account_catalog_unavailable"},
		{name: "unbound", model: "native", fast: "on", wantReason: "account_unbound"},
		{name: "stale", account: "device-codex", model: "native", fast: "on", stale: true, wantReason: "catalog_stale"},
		{name: "unknown fast", account: "device-codex", model: "small", fast: "on", wantReason: "fast_unknown"},
		{name: "invalid model", account: "device-codex", model: "missing", fast: "default", wantReason: "model_invalid"},
		{name: "invalid explicit effort", account: "device-codex", model: "native", effort: "turbo", fast: "default", wantReason: "effort_unsupported"},
		{name: "clear incompatible inherited effort", account: "device-codex", model: "native", effort: "turbo", fast: "default", clearEffort: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			now := time.Now()
			st := sqlitetest.MustOpen(t)
			rec, err := st.CreateSession(ctx, domain.SessionRecord{Kind: domain.KindWorker, Harness: domain.HarnessCodex, Mode: domain.SessionModeTUI, CreatedAt: now, UpdatedAt: now})
			if err != nil {
				t.Fatal(err)
			}
			if tc.account != "" {
				if err = st.BindSessionAgentAccount(ctx, rec.ID, domain.AgentAccountProviderCodex, tc.account, now); err != nil {
					t.Fatal(err)
				}
			}
			catalog := ports.AgentModelCatalog{AgentID: "codex", Models: []ports.AgentModelInfo{{ID: "native", IsDefault: true, Efforts: []string{"high"}, SupportsFast: true}, {ID: "small"}}}
			raw, _ := json.Marshal(catalog)
			observed := now
			if tc.stale {
				observed = now.AddDate(0, 0, -1)
			}
			if err = st.UpsertAgentModelCatalog(ctx, ports.CachedAgentModelCatalog{AgentID: "codex", CatalogJSON: string(raw), LastSuccessAt: observed, FetchedAt: observed}); err != nil {
				t.Fatal(err)
			}
			validator, ok := any(New(st, func() time.Time { return now })).(interface {
				ValidateSpawn(context.Context, domain.SessionID, domain.AgentSettings, bool) (domain.AgentSettings, error)
			})
			if !ok {
				t.Fatal("spawn capability preflight is unavailable")
			}
			got, err := validator.ValidateSpawn(ctx, rec.ID, domain.AgentSettings{Model: domain.AgentModelSelection(tc.model), Effort: domain.AgentEffortSelection(tc.effort), FastMode: domain.AgentFastMode(tc.fast)}, tc.clearEffort)
			if tc.wantReason != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantReason) {
					t.Fatalf("got %+v err=%v want reason=%s", got, err, tc.wantReason)
				}
				return
			}
			if err != nil || string(got.Effort) != tc.wantEffort {
				t.Fatalf("got %+v err=%v", got, err)
			}
		})
	}
}
