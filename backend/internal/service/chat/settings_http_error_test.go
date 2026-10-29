package chat_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd"
	"github.com/aoagents/agent-orchestrator/backend/internal/observe/sentryobs"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/agentsettings"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

type settingsHTTPEvents struct {
	mu     sync.Mutex
	events []ports.TelemetryEvent
}

type settingsCaptureTransport struct{ events chan *sentry.Event }

func (*settingsCaptureTransport) Configure(sentry.ClientOptions)        {}
func (*settingsCaptureTransport) Flush(time.Duration) bool              { return true }
func (*settingsCaptureTransport) FlushWithContext(context.Context) bool { return true }
func (*settingsCaptureTransport) Close()                                {}
func (t *settingsCaptureTransport) SendEvent(event *sentry.Event)       { t.events <- event }

func (a *settingsHTTPEvents) Emit(_ context.Context, e ports.TelemetryEvent) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, e)
}
func (*settingsHTTPEvents) Close(context.Context) error { return nil }
func TestSettingsHTTPRealSchedulerCASCancelAndIdentity(t *testing.T) {
	h, conv, _, _ := journalHarness(t)
	raw, _ := json.Marshal(ports.AgentModelCatalog{AgentID: "codex", Models: []ports.AgentModelInfo{{ID: "next", IsDefault: true, Efforts: []string{"high"}, SupportsFast: true}}})
	if err := h.st.UpsertAgentModelCatalog(t.Context(), ports.CachedAgentModelCatalog{AgentID: "codex", CatalogJSON: string(raw), InputFingerprint: "live-inputs", LastSuccessAt: h.now(), FetchedAt: h.now()}); err != nil {
		t.Fatal(err)
	}
	core := agentsettings.New(h.st, h.now)
	core.SetCatalogFingerprintSource(legacyCatalogInputs{})
	signer, _ := agentsettings.NewGrantAuthority([]byte(strings.Repeat("k", 32)), h.now)
	control := agentsettings.NewControl(core, h.st, agentsettings.NewAuthorizer(h.st, signer, h.now), h.svc, h.now)
	h.svc.SetAgentSettingsMutator(control)
	h.svc.SetAgentSettingsValidator(control)
	var logs bytes.Buffer
	events := &settingsHTTPEvents{}
	router := httpd.NewRouterWithControl(config.Config{}, slog.New(slog.NewTextHandler(&logs, nil)), nil, httpd.APIDeps{AgentSettings: core, AgentSettingsControl: control, Conversations: h.svc, Telemetry: events}, httpd.ControlDeps{})
	request := func(method, path, body, key, id, grant string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/api/v1/sessions/"+string(testSession)+path, strings.NewReader(body))
		req.Header.Set("X-Request-ID", "audit-request")
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		if id != "" {
			req.Header.Set("X-AO-Session-ID", id)
		}
		if grant != "" {
			req.Header.Set("X-AO-Settings-Grant", grant)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	first, err := h.ctrl.Send(t.Context(), ports.ChatUserMessage{Text: "busy"})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"expectedRevision":0,"settings":{"model":"next","effort":"high"}}`
	out := make(chan *httptest.ResponseRecorder, 2)
	var wg sync.WaitGroup
	for _, key := range []string{"cli", "ui"} {
		wg.Add(1)
		go func(k string) { defer wg.Done(); out <- request(http.MethodPatch, "/agent-settings", body, k, "", "") }(key)
	}
	wg.Wait()
	close(out)
	wins, conflicts := 0, 0
	var op domain.AgentSettingsOperation
	for w := range out {
		switch w.Code {
		case 202:
			wins++
			var res struct {
				Operation domain.AgentSettingsOperation `json:"operation"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
				t.Fatal(err)
			}
			op = res.Operation
		case 409:
			conflicts++
			if !strings.Contains(w.Body.String(), `"requestId":"audit-request"`) {
				t.Fatal(w.Body.String())
			}
		default:
			t.Fatalf("status=%d %s", w.Code, w.Body.String())
		}
	}
	if wins != 1 || conflicts != 1 || conv.stepCount() != 0 || op.Phase != domain.AgentSettingsPending {
		t.Fatalf("wins=%d conflicts=%d setters=%d", wins, conflicts, conv.stepCount())
	}
	stored, _, err := h.st.PendingAgentSettingsOperation(t.Context(), testSession)
	if err != nil {
		t.Fatal(err)
	}
	key := stored.IdempotencyKey
	if w := request(http.MethodPatch, "/agent-settings", body, key, "", ""); w.Code != 202 || !strings.Contains(w.Body.String(), op.ID) {
		t.Fatalf("retry %d %s", w.Code, w.Body.String())
	}
	if w := request(http.MethodGet, "/agent-settings/operations/"+op.ID, "", "", "", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"phase":"pending"`) {
		t.Fatalf("GET %d %s", w.Code, w.Body.String())
	}
	if w := request(http.MethodPatch, "/agent-settings", `{"settings":{"effort":"high"}}`, "missing", "", ""); w.Code != 400 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := request(http.MethodPost, "/agent-settings/operations/"+op.ID+"/cancel", fmt.Sprintf(`{"expectedVersion":%d}`, op.Version+1), "", "", ""); w.Code != 409 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := request(http.MethodPost, "/agent-settings/operations/"+op.ID+"/cancel", fmt.Sprintf(`{"expectedVersion":%d}`, op.Version), "", "", ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	canary := "CANARY_SETTINGS_CREDENTIAL_64"
	for _, path := range []string{"/agent-settings", "/conversation/settings"} {
		payload := body
		if path == "/conversation/settings" {
			payload = `{"model":"next"}`
		}
		w := request(http.MethodPatch, path, payload, "forged", "claim", canary)
		if w.Code != 403 || strings.Contains(w.Body.String(), canary) {
			t.Fatalf("identity fallback %s %d %s", path, w.Code, w.Body.String())
		}
	}
	rec, _, _ := h.st.GetSession(t.Context(), testSession)
	rec.Kind = domain.KindWorker
	if err := h.st.UpdateSession(t.Context(), rec); err != nil {
		t.Fatal(err)
	}
	actor, err := h.st.CreateSession(t.Context(), domain.SessionRecord{ProjectID: rec.ProjectID, Kind: domain.KindOrchestrator, Harness: domain.HarnessCodex, Mode: domain.SessionModeTUI, Metadata: domain.SessionMetadata{RuntimeLaunchID: "actor-gen"}, CreatedAt: h.now(), UpdatedAt: h.now()})
	if err != nil {
		t.Fatal(err)
	}
	token, err := signer.Issue(actor.ID, domain.AgentSettingsAuthority{ProjectID: rec.ProjectID, ActorHarness: domain.HarnessCodex, ActorMode: domain.SessionModeTUI, ActorGeneration: "actor-gen", IssuedAt: h.now().Unix(), ExpiresAt: h.now().Add(time.Hour).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	if w := request(http.MethodPatch, "/agent-settings", body, "no-policy", string(actor.ID), token); w.Code != 403 || !strings.Contains(w.Body.String(), "AGENT_SETTINGS_POLICY_DENIED") {
		t.Fatal(w.Code, w.Body.String())
	}
	p, _, _ := h.st.GetProject(t.Context(), string(rec.ProjectID))
	p.Config.AgentSettingsPolicy = &domain.AgentSettingsPolicy{Enabled: true, Harnesses: map[domain.AgentHarness]domain.AgentSettingsAllowance{domain.HarnessCodex: {Models: []domain.AgentModelSelection{"next"}, Efforts: []domain.AgentEffortSelection{"high"}}}}
	if err := h.st.UpsertProject(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	w := request(http.MethodPatch, "/agent-settings", body, "managed", string(actor.ID), token)
	if w.Code != 202 || strings.Contains(w.Body.String(), token) {
		t.Fatal(w.Code, w.Body.String())
	}
	pending, found, err := h.st.PendingAgentSettingsOperation(t.Context(), testSession)
	if err != nil || !found {
		t.Fatal(err)
	}
	saved, _ := json.Marshal(pending)
	eventJSON, _ := json.Marshal(events.events)
	for _, secret := range []string{canary, token} {
		for _, surface := range []string{logs.String(), string(saved), string(eventJSON)} {
			if strings.Contains(surface, secret) {
				t.Fatal("credential escaped")
			}
		}
	}
	conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: first.ProviderTurnID, TurnState: domain.TurnStateCompleted})
}

func TestSettingsRecoveryFailureDoesNotLeakGrantToHTTPLog(t *testing.T) {
	if sentryobs.Enabled() {
		t.Fatal("capture fixture requires an isolated disabled observer")
	}
	previousClient := sentry.CurrentHub().Client()
	capture := &settingsCaptureTransport{events: make(chan *sentry.Event, 8)}
	if err := sentryobs.Init(sentryobs.Config{DSN: "https://fake@example.invalid/1", Transport: capture}); err != nil {
		t.Fatal(err)
	}
	sentryobs.SetPolicyEnabled(true)
	t.Cleanup(func() {
		sentryobs.SetPolicyEnabled(false)
		_ = sentryobs.Init(sentryobs.Config{})
		sentry.CurrentHub().BindClient(previousClient)
	})
	h, _, _, _ := journalHarness(t)
	if err := h.svc.Stop(t.Context(), testSession); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(ports.AgentModelCatalog{AgentID: "codex", Models: []ports.AgentModelInfo{{ID: "next", IsDefault: true, Efforts: []string{"high"}, SupportsFast: true}}})
	if err := h.st.UpsertAgentModelCatalog(t.Context(), ports.CachedAgentModelCatalog{AgentID: "codex", CatalogJSON: string(raw), InputFingerprint: "live-inputs", LastSuccessAt: h.now(), FetchedAt: h.now()}); err != nil {
		t.Fatal(err)
	}
	core := agentsettings.New(h.st, h.now)
	core.SetCatalogFingerprintSource(legacyCatalogInputs{})
	signer, _ := agentsettings.NewGrantAuthority([]byte(strings.Repeat("k", 32)), h.now)
	conv := &journalConversation{fakeConversation: newFakeConversation()}
	conv.quiet.Store(true)
	var token string
	driver := fakeDriver{conv: conv, start: func(cfg ports.ChatStartConfig) (ports.ChatConversation, error) {
		token = cfg.Env[ports.EnvAgentSettingsGrant]
		return conv, nil
	}, resume: func(cfg ports.ChatResumeConfig) (ports.ChatConversation, error) {
		token = cfg.Env[ports.EnvAgentSettingsGrant]
		return conv, nil
	}}
	var logs bytes.Buffer
	events := &settingsHTTPEvents{}
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	svc := chatsvc.New(chatsvc.Options{NewID: uuid.NewString, Store: h.st, Sessions: h.st, Reader: fullSnapshotReader(h.st), PageReader: boundedSnapshotReader(h.st), Drivers: fakeRegistry{driver: driver}, Log: logger, Now: h.now})
	svc.SetAgentSettingsGrantEnvironment(agentsettings.NewGrantEnvironment(h.st, signer, h.now))
	control := agentsettings.NewControl(core, h.st, agentsettings.NewAuthorizer(h.st, signer, h.now), svc, h.now)
	svc.SetAgentSettingsMutator(control)
	svc.SetAgentSettingsValidator(control)
	_, err := svc.Start(t.Context(), chatsvc.StartConfig{SessionID: testSession, ProjectID: testProject, Harness: domain.HarnessCodex, WorkspacePath: t.TempDir(), ProviderConversationID: conv.ProviderConversationID()})
	if err != nil || token == "" {
		t.Fatalf("grant injection failed: %v", err)
	}
	t.Cleanup(func() { _ = svc.Stop(context.Background(), testSession) })
	conv.onStep = func(step domain.AgentSettingsCheckpoint) error {
		if step == domain.AgentSettingsApplyingFast || step == domain.AgentSettingsRollingBack {
			return errors.New("provider diagnostic echoed " + token)
		}
		return nil
	}
	router := httpd.NewRouterWithControl(config.Config{}, logger, nil, httpd.APIDeps{AgentSettings: core, AgentSettingsControl: control, Conversations: svc, Telemetry: events}, httpd.ControlDeps{})
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/sessions/"+string(testSession)+"/agent-settings", strings.NewReader(`{"expectedRevision":0,"settings":{"model":"next","effort":"high"}}`))
	req.Header.Set("Idempotency-Key", "secret-recovery")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	pending, found, err := h.st.PendingAgentSettingsOperation(t.Context(), testSession)
	if err != nil || !found || pending.Phase != domain.AgentSettingsRecoveryRequired {
		t.Fatalf("fault not reached: %d %+v %v", w.Code, pending, err)
	}
	durable, _ := json.Marshal(pending)
	telemetry, _ := json.Marshal(events.events)
	select {
	case event := <-capture.events:
		captured, err := json.Marshal(event)
		if err != nil || strings.Contains(string(captured), token) {
			t.Fatal("settings grant escaped through HTTP exception capture", err)
		}
	default:
		t.Fatal("HTTP 500 did not exercise the capture transport")
	}
	t.Logf("status=%d response_leak=%t durable_leak=%t telemetry_payload_leak=%t access_log_leak=%t", w.Code, strings.Contains(w.Body.String(), token), strings.Contains(string(durable), token), strings.Contains(string(telemetry), token), strings.Contains(logs.String(), token))
	if strings.Contains(logs.String(), token) || strings.Contains(w.Body.String(), token) || strings.Contains(string(durable), token) || strings.Contains(string(telemetry), token) {
		t.Fatal("settings grant escaped through failed provider recovery")
	}
}

// The background-task probe is provider I/O too; its error may contain
// private launch diagnostics before preparation or recovery can begin.
type settingsPrivateProbe struct{ *journalConversation }

func (*settingsPrivateProbe) CanHibernate(context.Context) (bool, error) {
	return false, errors.New("PRIVATE_SETTINGS_PROBE_DIAGNOSTIC")
}

func TestSettingsProbeDoesNotExposePrivateProviderDiagnostic(t *testing.T) {
	conv := &settingsPrivateProbe{&journalConversation{fakeConversation: newFakeConversation()}}
	conv.quiet.Store(true)
	h := newHarnessWithConversationAndStoreForHarness(t, conv, func(st *sqlite.Store) chatsvc.Store {
		if err := st.BindSessionAgentAccount(t.Context(), testSession, domain.AgentAccountProviderCodex, "device-codex", time.Now()); err != nil {
			t.Fatal(err)
		}
		return st
	}, domain.HarnessCodex)
	rec, _, err := h.st.GetSession(t.Context(), testSession)
	if err != nil {
		t.Fatal(err)
	}
	rec.Metadata.ProviderConversationID = conv.ProviderConversationID()
	if err := h.st.UpdateSession(t.Context(), rec); err != nil {
		t.Fatal(err)
	}
	h.svc.SetAgentSettingsValidator(&journalValidator{})
	_, err = h.ctrl.QueueAgentSettings(t.Context(), journalIntent(t, h))
	if err == nil || strings.Contains(err.Error(), "PRIVATE_SETTINGS_PROBE_DIAGNOSTIC") {
		t.Fatal("settings probe exposed the raw provider diagnostic")
	}
}
