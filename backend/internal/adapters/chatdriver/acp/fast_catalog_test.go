package acp

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// Match the pinned Claude bridge: fast disappears on a non-supporting model,
// but its user intent survives; attempting the absent setter is an error.
type fastCatalogAgent struct {
	*fakeAgent
	configMu    sync.Mutex
	model, fast string
	boolean     bool
	fastCalls   int
}

func (a *fastCatalogAgent) catalogLocked() []acpsdk.SessionConfigOption {
	options := []acpsdk.SessionConfigOption{selectConfigOption("model", "Model", "model", a.model, "capable", "plain")}
	if a.model == "capable" {
		if a.boolean {
			options = append(options, booleanConfigOption("fast", "Fast", a.fast == "on"))
		} else {
			options = append(options, selectConfigOption("fast", "Fast", "", a.fast, "on", "off"))
		}
	}
	return options
}

func (a *fastCatalogAgent) NewSession(context.Context, acpsdk.NewSessionRequest) (acpsdk.NewSessionResponse, error) {
	a.configMu.Lock()
	defer a.configMu.Unlock()
	return acpsdk.NewSessionResponse{SessionId: "claude-session-1", ConfigOptions: a.catalogLocked()}, nil
}
func (a *fastCatalogAgent) LoadSession(context.Context, acpsdk.LoadSessionRequest) (acpsdk.LoadSessionResponse, error) {
	a.configMu.Lock()
	defer a.configMu.Unlock()
	return acpsdk.LoadSessionResponse{ConfigOptions: a.catalogLocked()}, nil
}
func (a *fastCatalogAgent) ResumeSession(context.Context, acpsdk.ResumeSessionRequest) (acpsdk.ResumeSessionResponse, error) {
	a.configMu.Lock()
	defer a.configMu.Unlock()
	return acpsdk.ResumeSessionResponse{ConfigOptions: a.catalogLocked()}, nil
}
func (a *fastCatalogAgent) SetSessionConfigOption(_ context.Context, p acpsdk.SetSessionConfigOptionRequest) (acpsdk.SetSessionConfigOptionResponse, error) {
	a.configMu.Lock()
	defer a.configMu.Unlock()
	if a.configErr != nil {
		return acpsdk.SetSessionConfigOptionResponse{}, a.configErr
	}
	id, value := "", ""
	if p.ValueId != nil {
		id, value = string(p.ValueId.ConfigId), string(p.ValueId.Value)
	}
	if p.Boolean != nil {
		id = string(p.Boolean.ConfigId)
		value = "off"
		if p.Boolean.Value {
			value = "on"
		}
	}
	switch id {
	case "model":
		a.model = value
	case "fast":
		a.fastCalls++
		if a.model != "capable" {
			return acpsdk.SetSessionConfigOptionResponse{}, errors.New("Unknown config option: fast")
		}
		a.fast = value
	default:
		return acpsdk.SetSessionConfigOptionResponse{}, errors.New("Unknown config option")
	}
	return acpsdk.SetSessionConfigOptionResponse{ConfigOptions: a.catalogLocked()}, nil
}

func TestAdvertisedFastSetterErrorsRemainVisible(t *testing.T) {
	a := &fastCatalogAgent{fakeAgent: &fakeAgent{configErr: errors.New("provider refused fast")}, model: "capable", fast: "off"}
	d := newFastCatalogDriver(t, a)
	_, err := d.Start(t.Context(), ports.ChatStartConfig{WorkspacePath: t.TempDir(), FastMode: domain.AgentFastOn})
	if err == nil || !strings.Contains(err.Error(), "provider refused fast") {
		t.Fatalf("advertised setter rejection was suppressed: %v", err)
	}
}

func newFastCatalogDriver(t *testing.T, a *fastCatalogAgent) *Driver {
	t.Helper()
	d := New(Config{Harness: domain.HarnessClaudeCode, Capabilities: ports.ChatCapabilities{ports.ChatCapabilityStreaming: true}, Probe: func(context.Context) error { return nil }, Launch: func(context.Context, LaunchConfig) (Launch, error) { return Launch{Command: "fake"}, nil }, SessionOptions: func(s ports.ChatTurnSettings) []SessionOption {
		var options []SessionOption
		if s.Model != "" {
			options = append(options, SessionOption{ID: "model", Value: s.Model})
		}
		if s.FastMode == domain.AgentFastOn || s.FastMode == domain.AgentFastOff {
			options = append(options, SessionOption{ID: "fast", Value: string(s.FastMode), IfAdvertised: true})
		}
		return options
	}}, slog.New(slog.DiscardHandler))
	d.useTestProcess(func(Launch, string) (*process, error) {
		clientR, clientW := io.Pipe()
		agentR, agentW := io.Pipe()
		a.conn = acpsdk.NewAgentSideConnection(a, agentW, clientR)
		var once sync.Once
		return &process{stdin: clientW, stdout: agentR, stop: func() error {
			once.Do(func() { _ = clientW.Close(); _ = clientR.Close(); _ = agentW.Close(); _ = agentR.Close() })
			return nil
		}}, nil
	})
	return d
}

func TestFastPreferenceSkipsMissingModelOptionAndReturns(t *testing.T) {
	for _, mode := range []domain.AgentFastMode{domain.AgentFastOn, domain.AgentFastOff} {
		for _, boolean := range []bool{false, true} {
			name := string(mode) + "/select"
			if boolean {
				name = string(mode) + "/boolean"
			}
			t.Run(name, func(t *testing.T) {
				a := &fastCatalogAgent{fakeAgent: &fakeAgent{promptNoPermission: true}, model: "capable", fast: string(mode), boolean: boolean}
				d := newFastCatalogDriver(t, a)
				conv, err := d.Start(t.Context(), ports.ChatStartConfig{WorkspacePath: t.TempDir(), Model: "capable", FastMode: mode})
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = conv.Close() }()
				send := func(model string) {
					t.Helper()
					ref, err := conv.SendTurn(t.Context(), ports.ChatUserMessage{Text: "next", Settings: ports.ChatTurnSettings{Model: model, FastMode: mode}})
					if err != nil {
						t.Fatalf("send on %s with saved fast %s: %v", model, mode, err)
					}
					if err := conv.(ports.ChatDeferredTurnStarter).StartDeferredTurn(ref.ProviderTurnID); err != nil {
						t.Fatal(err)
					}
					event := nextEvent(t, conv.Events())
					for event.Kind != ports.ChatEventTurnCompleted {
						event = nextEvent(t, conv.Events())
					}
				}
				a.configMu.Lock()
				before := a.fastCalls
				a.configMu.Unlock()
				// The same turn's model setter replaces the catalog before fast applies.
				send("plain")
				a.configMu.Lock()
				calls, intent := a.fastCalls, a.fast
				a.configMu.Unlock()
				if calls != before || intent != string(mode) {
					t.Fatalf("missing option invoked setter or lost intent: calls=%d/%d intent=%s", calls, before, intent)
				}
				send("capable")
				a.configMu.Lock()
				intent = a.fast
				a.configMu.Unlock()
				if intent != string(mode) {
					t.Fatalf("returning fast model lost saved preference: %s", intent)
				}
			})
		}
	}
}

func TestFastPreferenceOpensAndResumesWithoutAdvertisedOption(t *testing.T) {
	for _, mode := range []domain.AgentFastMode{domain.AgentFastOn, domain.AgentFastOff} {
		for _, resume := range []bool{false, true} {
			name := string(mode) + "/start"
			if resume {
				name = string(mode) + "/resume"
			}
			t.Run(name, func(t *testing.T) {
				a := &fastCatalogAgent{fakeAgent: &fakeAgent{promptNoPermission: true}, model: "plain", fast: string(mode)}
				d := newFastCatalogDriver(t, a)
				var conv ports.ChatConversation
				var err error
				if resume {
					conv, err = d.Resume(t.Context(), ports.ChatResumeConfig{WorkspacePath: t.TempDir(), ProviderConversationID: "claude-session-1", Model: "plain", FastMode: mode})
				} else {
					conv, err = d.Start(t.Context(), ports.ChatStartConfig{WorkspacePath: t.TempDir(), Model: "plain", FastMode: mode})
				}
				if err != nil {
					t.Fatalf("open with absent fast option: %v", err)
				}
				defer func() { _ = conv.Close() }()
				a.configMu.Lock()
				calls, intent := a.fastCalls, a.fast
				a.configMu.Unlock()
				if calls != 0 || intent != string(mode) {
					t.Fatalf("open sent absent fast setter or lost preference: calls=%d intent=%s", calls, intent)
				}
			})
		}
	}
}
