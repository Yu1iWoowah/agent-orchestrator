package agentsettings

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

type scopedControlInputs struct {
	values map[string]string
	seen   []string
}

func (s *scopedControlInputs) CatalogFingerprint(_ context.Context, _ string, scope string) (string, error) {
	s.seen = append(s.seen, scope)
	return s.values[scope], nil
}

func scopedControlFixture(t *testing.T, harness domain.AgentHarness, managed, catalog bool) (*Control, *sqlite.Store, domain.SessionRecord, *scopedControlInputs, string) {
	t.Helper()
	c, st, rec := controlFixture(t)
	rec.Harness = harness
	if err := st.UpdateSession(t.Context(), rec); err != nil {
		t.Fatal(err)
	}
	provider, _ := domain.AgentAccountProviderFor(harness)
	accountID := domain.DeviceAgentAccountID(provider)
	if managed {
		accountID = "managed-a"
		if err := st.CreateAgentAccount(t.Context(), domain.AgentAccount{ID: accountID, Provider: provider, Home: t.TempDir(), ProfileKind: domain.AgentAccountProfileExternal, CreatedAt: c.now()}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.BindSessionAgentAccount(t.Context(), rec.ID, provider, accountID, c.now()); err != nil {
		t.Fatal(err)
	}
	scope := ports.AccountModelCatalogScope(accountID)
	inputs := &scopedControlInputs{values: map[string]string{"": "device-fingerprint", ports.AccountModelCatalogScope("managed-a"): "managed-fingerprint"}}
	c.core.SetCatalogFingerprintSource(inputs)
	raw, _ := json.Marshal(ports.AgentModelCatalog{AgentID: string(harness), Models: []ports.AgentModelInfo{{ID: "native", IsDefault: true, Efforts: []string{"low", "high"}, SupportsFast: true}}})
	for _, key := range []string{"", scope} {
		if key != "" && !catalog {
			continue
		}
		if err := st.UpsertAgentModelCatalog(t.Context(), ports.CachedAgentModelCatalog{AgentID: string(harness), ProjectID: key, CatalogJSON: string(raw), InputFingerprint: inputs.values[key], LastSuccessAt: c.now(), FetchedAt: c.now()}); err != nil {
			t.Fatal(err)
		}
	}
	return c, st, rec, inputs, scope
}

func TestControlUsesPreflightAccountScope(t *testing.T) {
	for _, harness := range []domain.AgentHarness{domain.HarnessCodex, domain.HarnessClaudeCode} {
		for _, managed := range []bool{false, true} {
			name := string(harness) + "/device"
			if managed {
				name = string(harness) + "/managed"
			}
			t.Run(name, func(t *testing.T) {
				c, st, rec, inputs, scope := scopedControlFixture(t, harness, managed, true)
				selection := domain.AgentSettings{Model: "native", Effort: "high", FastMode: domain.AgentFastDefault}
				if _, err := c.core.ValidateSpawn(t.Context(), rec.ID, selection, false); err != nil {
					t.Fatal("account-scoped preflight failed before Control", err)
				}
				zero := int64(0)
				high := domain.AgentEffortSelection("high")
				op, err := c.Change(t.Context(), Caller{}, rec.ID, Change{ExpectedRevision: &zero, IdempotencyKey: "account-scope", Settings: domain.AgentSettingsPatch{Effort: &high}})
				if err != nil || op.Phase != domain.AgentSettingsPending {
					t.Fatal("Control rejected the validated account scope", err)
				}
				current, err := st.SessionAgentSettings(t.Context(), rec.ID)
				if err != nil {
					t.Fatal(err)
				}
				owner := domain.AgentSettingsOwner{SessionID: current.SessionID, AccountID: current.AccountID, ControllerGeneration: current.OwnerGeneration, ProviderConversationID: current.ProviderConversationID, BranchID: current.BranchID}
				proof, err := c.ValidateAgentSettings(t.Context(), op, owner, false)
				if err != nil || proof.InputFingerprint != inputs.values[scope] {
					t.Fatalf("runtime qualification used a different account: %+v %v", proof, err)
				}
				for _, received := range inputs.seen {
					if received != scope {
						t.Fatalf("preflight/proof crossed account scopes: got %q want %q", received, scope)
					}
				}
				inputs.values[scope] = "changed-inputs"
				if _, err := c.ValidateAgentSettings(t.Context(), op, owner, false); !errors.Is(err, ports.ErrAgentSettingsCapabilitiesUnavailable) {
					t.Fatalf("changed account/binary inputs were accepted: %v", err)
				}
				owner.AccountID = "another-account"
				if _, err := c.ValidateAgentSettings(t.Context(), op, owner, false); !errors.Is(err, ports.ErrAgentSettingsConflict) {
					t.Fatalf("foreign account owner was accepted: %v", err)
				}
			})
		}
	}
}

func TestControlDoesNotBorrowDeviceCatalogForManagedAccount(t *testing.T) {
	for _, harness := range []domain.AgentHarness{domain.HarnessCodex, domain.HarnessClaudeCode} {
		t.Run(string(harness), func(t *testing.T) {
			c, st, rec, _, _ := scopedControlFixture(t, harness, true, false)
			zero := int64(0)
			high := domain.AgentEffortSelection("high")
			if _, err := c.Change(t.Context(), Caller{}, rec.ID, Change{ExpectedRevision: &zero, IdempotencyKey: "missing-scoped-catalog", Settings: domain.AgentSettingsPatch{Effort: &high}}); !hasCode(err, "AGENT_SETTINGS_CAPABILITIES_UNAVAILABLE") {
				t.Fatalf("managed account borrowed the fresh device catalog: %v", err)
			}
			if _, found, err := st.PendingAgentSettingsOperation(t.Context(), rec.ID); err != nil || found {
				t.Fatal("rejected account created pending intent", err)
			}
		})
	}
}
