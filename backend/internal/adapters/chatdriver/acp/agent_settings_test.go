package acp

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"sync"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	acpsdk "github.com/coder/acp-go-sdk"
)

type settingsAgent struct {
	*fakeAgent
	configMu                        sync.Mutex
	model, effort, fast, permission string
	calls                           []string
	omit, clamp                     bool
}

func (a *settingsAgent) catalog() []acpsdk.SessionConfigOption {
	efforts := []string{"high", "low", "default"}
	if a.model == "new" {
		efforts = []string{"medium", "low", "default"}
	}
	return []acpsdk.SessionConfigOption{
		selectConfigOption("model", "Model", "model", a.model, "old", "new", "default"),
		selectConfigOption("effort", "Effort", "thought_level", a.effort, efforts...),
		selectConfigOption("fast", "Fast", "", a.fast, "on", "off"),
		selectConfigOption("mode", "Mode", "", a.permission, "auto", "default"),
	}
}
func (a *settingsAgent) NewSession(context.Context, acpsdk.NewSessionRequest) (acpsdk.NewSessionResponse, error) {
	a.configMu.Lock()
	defer a.configMu.Unlock()
	return acpsdk.NewSessionResponse{SessionId: "settings-native", ConfigOptions: a.catalog()}, nil
}
func (a *settingsAgent) SetSessionConfigOption(_ context.Context, p acpsdk.SetSessionConfigOptionRequest) (acpsdk.SetSessionConfigOptionResponse, error) {
	a.configMu.Lock()
	defer a.configMu.Unlock()
	id, value := string(p.ValueId.ConfigId), string(p.ValueId.Value)
	a.calls = append(a.calls, id+"="+value)
	switch id {
	case "model":
		a.model = value
		if value == "new" {
			a.effort = "medium"
			if a.clamp {
				a.permission = "default"
			}
		}
	case "effort":
		a.effort = value
	case "fast":
		a.fast = value
	case "mode":
		a.permission = value
	default:
		return acpsdk.SetSessionConfigOptionResponse{}, errors.New("unexpected setter")
	}
	if a.omit {
		return acpsdk.SetSessionConfigOptionResponse{}, nil
	}
	return acpsdk.SetSessionConfigOptionResponse{ConfigOptions: a.catalog()}, nil
}

func settingsConversation(t *testing.T, a *settingsAgent) ports.ChatConversation {
	t.Helper()
	d := New(Config{Harness: domain.HarnessClaudeCode, AgentSettingsDefaults: map[string]string{"model": "default", "effort": "default"}, Capabilities: ports.ChatCapabilities{ports.ChatCapabilityStreaming: true}, Probe: func(context.Context) error { return nil }, Launch: func(context.Context, LaunchConfig) (Launch, error) { return Launch{Command: "fake"}, nil }}, slog.New(slog.DiscardHandler))
	d.useTestProcess(func(Launch, string) (*process, error) {
		cr, cw := io.Pipe()
		ar, aw := io.Pipe()
		a.conn = acpsdk.NewAgentSideConnection(a, aw, cr)
		var once sync.Once
		return &process{stdin: cw, stdout: ar, stop: func() error {
			once.Do(func() { _ = cw.Close(); _ = cr.Close(); _ = aw.Close(); _ = ar.Close() })
			return nil
		}}, nil
	})
	c, err := d.Start(t.Context(), ports.ChatStartConfig{WorkspacePath: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestJournalledACPSettingsUsesPostModelCatalogAndOrderedReceipts(t *testing.T) {
	a := &settingsAgent{fakeAgent: &fakeAgent{}, model: "old", effort: "high", fast: "on", permission: "auto"}
	c := settingsConversation(t, a)
	staged, ok := c.(ports.ChatAgentSettingsController)
	if !ok {
		t.Fatal("ACP has no strict journalled settings executor")
	}
	source := domain.AgentSettings{Model: "old", Effort: "high", FastMode: domain.AgentFastOn}
	target := domain.AgentSettings{Model: "new", Effort: "low", FastMode: domain.AgentFastOff}
	plan, err := staged.PrepareAgentSettings(t.Context(), source, target)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.calls) != 0 {
		t.Fatal("preparation called provider setters")
	}
	for _, step := range []domain.AgentSettingsCheckpoint{domain.AgentSettingsApplyingModel, domain.AgentSettingsApplyingEffort, domain.AgentSettingsApplyingFast, domain.AgentSettingsVerifying} {
		if err := staged.ApplyAgentSettingsStep(t.Context(), plan, step, false); err != nil {
			t.Fatalf("%s: %v", step, err)
		}
	}
	a.configMu.Lock()
	defer a.configMu.Unlock()
	if !reflect.DeepEqual(a.calls, []string{"model=new", "effort=low", "fast=off"}) {
		t.Fatalf("wrong provider order: %v", a.calls)
	}
	if a.permission != "auto" {
		t.Fatal("settings changed permission")
	}
}

func TestJournalledACPSettingsRejectsUnprovenCatalogAndPermissionChange(t *testing.T) {
	for _, mode := range []string{"missing_catalog", "permission", "new_effort"} {
		t.Run(mode, func(t *testing.T) {
			a := &settingsAgent{fakeAgent: &fakeAgent{}, model: "old", effort: "high", fast: "on", permission: "auto", omit: mode == "missing_catalog", clamp: mode == "permission"}
			c := settingsConversation(t, a)
			staged, ok := c.(ports.ChatAgentSettingsController)
			if !ok {
				t.Fatal("ACP has no strict journalled settings executor")
			}
			source := domain.AgentSettings{Model: "old", Effort: "high", FastMode: domain.AgentFastOn}
			target := domain.AgentSettings{Model: "new", Effort: "low", FastMode: domain.AgentFastOff}
			if mode == "new_effort" {
				target.Effort = "high"
			}
			plan, err := staged.PrepareAgentSettings(t.Context(), source, target)
			if err != nil {
				t.Fatal(err)
			}
			err = staged.ApplyAgentSettingsStep(t.Context(), plan, domain.AgentSettingsApplyingModel, false)
			if mode == "new_effort" && err == nil {
				err = staged.ApplyAgentSettingsStep(t.Context(), plan, domain.AgentSettingsApplyingEffort, false)
			}
			if err == nil {
				t.Fatal("unproven model-dependent settings were accepted")
			}
			a.configMu.Lock()
			defer a.configMu.Unlock()
			if !reflect.DeepEqual(a.calls, []string{"model=new"}) {
				t.Fatalf("dependent setter escaped invalid catalog: %v", a.calls)
			}
		})
	}
}

func TestJournalledACPSettingsCannotGuessFastDefault(t *testing.T) {
	a := &settingsAgent{fakeAgent: &fakeAgent{}, model: "old", effort: "high", fast: "on", permission: "auto"}
	c := settingsConversation(t, a)
	staged, ok := c.(ports.ChatAgentSettingsController)
	if !ok {
		t.Fatal("ACP has no strict journalled settings executor")
	}
	source := domain.AgentSettings{Model: "old", Effort: "high", FastMode: domain.AgentFastOn}
	target := source
	target.FastMode = domain.AgentFastDefault
	_, err := staged.PrepareAgentSettings(t.Context(), source, target)
	if !errors.Is(err, ports.ErrAgentSettingsUnsupported) {
		t.Fatalf("unprovable fast reset accepted: %v", err)
	}
	if len(a.calls) != 0 {
		t.Fatal("unsupported reset touched native provider")
	}
}

func TestJournalledACPSettingsResetAndRollbackUseProviderReceipts(t *testing.T) {
	for _, mode := range []string{"reset", "native_fast", "permission_rollback"} {
		t.Run(mode, func(t *testing.T) {
			a := &settingsAgent{fakeAgent: &fakeAgent{}, model: "old", effort: "high", fast: "on", permission: "auto", clamp: mode == "permission_rollback"}
			c := settingsConversation(t, a)
			staged := c.(ports.ChatAgentSettingsController)
			source := domain.AgentSettings{Model: "old", Effort: "high", FastMode: domain.AgentFastOn}
			target := domain.AgentSettings{FastMode: domain.AgentFastOn}
			if mode == "native_fast" {
				source.FastMode = domain.AgentFastDefault
				target = source
				target.FastMode = domain.AgentFastOff
			}
			if mode == "permission_rollback" {
				target = domain.AgentSettings{Model: "new", Effort: "low", FastMode: domain.AgentFastOff}
			}
			plan, err := staged.PrepareAgentSettings(t.Context(), source, target)
			if err != nil {
				t.Fatal(err)
			}
			for _, step := range []domain.AgentSettingsCheckpoint{domain.AgentSettingsApplyingModel, domain.AgentSettingsApplyingEffort, domain.AgentSettingsApplyingFast, domain.AgentSettingsVerifying} {
				err = staged.ApplyAgentSettingsStep(t.Context(), plan, step, false)
				if mode == "permission_rollback" {
					if !errors.Is(err, ports.ErrAgentSettingsConflict) {
						t.Fatalf("permission silently clamped: %v", err)
					}
					break
				}
				if err != nil {
					t.Fatalf("%s: %v", step, err)
				}
			}
			if mode == "reset" && (a.model != "default" || a.effort != "default") {
				t.Fatal("explicit selections were not reset")
			}
			if mode == "native_fast" && a.fast != "off" {
				t.Fatal("native default fast was not explicitly disabled")
			}
			if err := staged.ApplyAgentSettingsStep(t.Context(), plan, domain.AgentSettingsRollingBack, true); err != nil {
				t.Fatal(err)
			}
			a.configMu.Lock()
			defer a.configMu.Unlock()
			if a.model != "old" || a.effort != "high" || a.fast != "on" || a.permission != "auto" {
				t.Fatalf("unproven restoration: %s/%s/%s/%s", a.model, a.effort, a.fast, a.permission)
			}
		})
	}
}
