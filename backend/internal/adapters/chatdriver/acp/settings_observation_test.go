package acp

import (
	"testing"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestProviderFastObservationUsesReceiptNotSpawnPreference(t *testing.T) {
	a := &fastCatalogAgent{fakeAgent: &fakeAgent{}, model: "plain", fast: "on"}
	d := newFastCatalogDriver(t, a)
	conv, err := d.Start(t.Context(), ports.ChatStartConfig{WorkspacePath: t.TempDir(), FastMode: domain.AgentFastOn})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conv.Close() }()
	observer, ok := conv.(ports.ChatAgentSettingsObserver)
	if !ok {
		t.Fatal("provider receipts are unavailable")
	}
	got := observer.AgentSettingsObservation()
	if got.FastMode.State != domain.AgentSettingNotProvided || got.FastMode.Value != "" || got.ObservedAt == nil {
		t.Fatalf("spawn fast on claimed effective on without advertised fast: %+v", got)
	}
}

func TestEarlySettingsReceiptCannotCrossProviderSession(t *testing.T) {
	a := &fastCatalogAgent{fakeAgent: &fakeAgent{}, model: "plain", fast: "off"}
	conv, err := newFastCatalogDriver(t, a).Start(t.Context(), ports.ChatStartConfig{WorkspacePath: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conv.Close() }()
	c := conv.(*conversation)
	c.mu.Lock()
	c.sessionID = ""
	c.mu.Unlock()
	if err := c.processUpdate(acpsdk.SessionNotification{SessionId: "other-session", Update: acpsdk.SessionUpdate{ConfigOptionUpdate: &acpsdk.SessionConfigOptionUpdate{ConfigOptions: []acpsdk.SessionConfigOption{booleanConfigOption("fast", "Fast", true)}}}}); err != nil {
		t.Fatal(err)
	}
	c.start("claude-session-1", ports.ChatCapabilities{}, nil, nil, nil, "", nil, nil, nil, nil)
	if got := c.AgentSettingsObservation(); got.FastMode.State != domain.AgentSettingUnknown || got.ObservedAt != nil {
		t.Fatalf("early receipt crossed session identity: %+v", got)
	}
}

func TestProviderObservationRequiresAuthoritativeConfirmation(t *testing.T) {
	for _, mode := range []domain.AgentFastMode{domain.AgentFastOn, domain.AgentFastOff} {
		for _, boolean := range []bool{false, true} {
			t.Run(string(mode)+"/"+map[bool]string{false: "select", true: "boolean"}[boolean], func(t *testing.T) {
				a := &fastCatalogAgent{fakeAgent: &fakeAgent{}, model: "capable", fast: string(mode), boolean: boolean}
				d := newFastCatalogDriver(t, a)
				conv, err := d.Start(t.Context(), ports.ChatStartConfig{WorkspacePath: t.TempDir(), FastMode: mode})
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = conv.Close() }()
				c := conv.(*conversation)
				got := c.AgentSettingsObservation()
				if got.FastMode.State != domain.AgentSettingKnown || got.FastMode.Value != string(mode) {
					t.Fatalf("reported current preference: %+v", got)
				}
				c.applyAcceptedConfigOption("fast", ports.ChatConfigOptionValue{Select: "on"})
				got = c.AgentSettingsObservation()
				if got.FastMode.State != domain.AgentSettingUnknown || got.ObservedAt != nil {
					t.Fatalf("optimistic local value became evidence: %+v", got)
				}
				c.replaceConfigOptions(nil)
				got = c.AgentSettingsObservation()
				if got.FastMode.State != domain.AgentSettingNotProvided || got.ObservedAt == nil {
					t.Fatalf("explicit empty catalog was not authoritative: %+v", got)
				}
			})
		}
	}
}
