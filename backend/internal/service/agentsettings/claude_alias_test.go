package agentsettings

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestStrictSpawnClaudeAlias(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name, model, effort, current string
		clear                        bool
		concrete                     []ports.AgentModelInfo
		want                         error
	}{
		{name: "default", effort: "xhigh", current: "saved", concrete: []ports.AgentModelInfo{{ID: "claude-opus-5-5", Efforts: []string{"low", "xhigh"}}}},
		{name: "explicit", model: "opus", effort: "low", current: "saved", concrete: []ports.AgentModelInfo{{ID: "claude-opus-5-5", Efforts: []string{"low", "xhigh"}}}},
		{name: "unsupported", model: "opus", effort: "max", current: "saved", concrete: []ports.AgentModelInfo{{ID: "claude-opus-5-5", Efforts: []string{"low", "xhigh"}}}, want: ports.ErrAgentSettingsUnsupported},
		{name: "unknown", model: "opus", effort: "xhigh", current: "saved", concrete: []ports.AgentModelInfo{{ID: "custom", Label: "Opus 5.5", Efforts: []string{"xhigh"}}}, want: ports.ErrAgentSettingsCapabilitiesUnavailable},
		{name: "unknown inherited", model: "opus", effort: "xhigh", current: "saved", clear: true, want: ports.ErrAgentSettingsCapabilitiesUnavailable},
		{name: "fingerprint gate", model: "opus", effort: "xhigh", current: "changed", concrete: []ports.AgentModelInfo{{ID: "claude-opus-5-5", Efforts: []string{"xhigh"}}}, want: ports.ErrAgentSettingsCapabilitiesUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(ports.AgentModelCatalog{AgentID: "claude-code", Models: append([]ports.AgentModelInfo{{ID: "opus", IsDefault: true}}, tc.concrete...)})
			st := catalogEvidenceStore{settings: domain.SessionAgentSettings{SessionID: "worker", Harness: domain.HarnessClaudeCode, AccountID: "device-claude"}, catalog: ports.CachedAgentModelCatalog{AgentID: "claude-code", CatalogJSON: string(raw), LastSuccessAt: now, FetchedAt: now, RefreshState: "idle", InputFingerprint: "saved"}}
			svc := New(st, func() time.Time { return now })
			svc.SetCatalogFingerprintSource(fixedCatalogFingerprint(tc.current))
			selected := domain.AgentSettings{FastMode: domain.AgentFastDefault, Model: domain.AgentModelSelection(tc.model), Effort: domain.AgentEffortSelection(tc.effort)}
			got, err := svc.ValidateSpawn(context.Background(), "worker", selected, tc.clear)
			if tc.want != nil {
				if !errors.Is(err, tc.want) {
					t.Fatalf("got %+v err=%v, want %v", got, err, tc.want)
				}
				return
			}
			if err != nil || got != selected {
				t.Fatalf("got %+v err=%v, want %+v", got, err, selected)
			}
		})
	}
}
