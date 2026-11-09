package httpd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	rcasvc "github.com/aoagents/agent-orchestrator/backend/internal/service/rca"
	usagesvc "github.com/aoagents/agent-orchestrator/backend/internal/service/usage"
)

// Both consumers use the existing metadata/ledger read ports. No fake replaces
// the EventsReader, resolver, streaming reader, numeric store or controller.
type rcaFlowMetadata struct {
	rec        domain.SessionRecord
	off, fail  bool
	beforeList func(context.Context)
}

func (m *rcaFlowMetadata) GetProject(context.Context, string) (domain.ProjectRecord, bool, error) {
	on := !m.off
	return domain.ProjectRecord{ID: "agent-orchestrator", Config: domain.ProjectConfig{RCA: domain.RCAConfig{Enabled: &on}}}, true, nil
}
func (m *rcaFlowMetadata) GetSession(_ context.Context, id domain.SessionID) (domain.SessionRecord, bool, error) {
	return m.rec, id == m.rec.ID, nil
}
func (m *rcaFlowMetadata) ListSessions(ctx context.Context, _ domain.ProjectID) ([]domain.SessionRecord, error) {
	if m.beforeList != nil {
		m.beforeList(ctx)
	}
	return []domain.SessionRecord{m.rec}, nil
}
func (*rcaFlowMetadata) ListUsageBindingsForSession(context.Context, domain.SessionID) ([]domain.UsageBindingRecord, error) {
	return nil, nil
}
func (*rcaFlowMetadata) ListUsageSourcesForBinding(context.Context, int64) ([]domain.UsageSourceRecord, error) {
	return nil, nil
}
func (*rcaFlowMetadata) SessionAgentSettings(context.Context, domain.SessionID) (domain.SessionAgentSettings, error) {
	return domain.SessionAgentSettings{}, nil
}
func (*rcaFlowMetadata) GetUsageEventsThroughID(context.Context, domain.SessionID) (int64, error) {
	return 1, nil
}
func (m *rcaFlowMetadata) ListUsageEventsPage(context.Context, domain.SessionID, int64, int64, int64) ([]domain.UsageLedgerEvent, error) {
	if m.fail {
		return nil, errors.New("A_SYNTHETIC_CREDENTIAL_CANARY B_SYNTHETIC_CONTENT_CANARY")
	}
	n := int64(42)
	return []domain.UsageLedgerEvent{{ID: 1, BindingID: 2, UsageSourceID: 3, ModelUsageEvent: domain.ModelUsageEvent{SourceEventKey: "A_SYNTHETIC_LEDGER_KEY", ModelID: "B_SYNTHETIC_MODEL", Tokens: domain.UsageTokenMetrics{InputTokens: &n}}}}, nil
}

func TestRCAActualHTTPFlowCanariesAndCanonicalControl(t *testing.T) {
	for _, scenario := range []string{"normal", "storage_failure", "off", "origin_rejected", "source_rejected", "bad_json", "bad_schema", "bad_control_json", "bad_control_schema", "depth_limit", "line_limit", "window_limit", "read_failure", "cancel_after_read", "background_panic", "storage_corrupt", "service_5xx"} {
		t.Run(scenario, func(t *testing.T) {
			fault := scenario == "storage_failure"
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			nativeID := "11111111-1111-4111-8111-111111111111"
			root := filepath.Join(home, ".codex", "sessions")
			if err := os.MkdirAll(root, 0700); err != nil {
				t.Fatal(err)
			}
			canaries := []string{"A_SYNTHETIC_CREDENTIAL_CANARY", "B_SYNTHETIC_CONTENT_CANARY"}
			source := filepath.Join(root, "rollout-lab-"+nativeID+".jsonl")
			both := canaries[0] + " " + canaries[1]
			body := `{"type":"session_meta","payload":{"id":"` + nativeID + `","cli_version":"0.167.0","source":"cli","cwd":"/synthetic/` + both + `","base_instructions":{"text":"API_KEY=` + both + `"},"dynamic_tools":[{"name":"private_tool","description":"` + both + `","parameters":{"type":"object","description":"` + both + `"}}]}}` + "\n" +
				`{"type":"response_item","payload":{"type":"message","role":"user","id":"` + both + `","content":[{"type":"input_text","text":"prompt/code/.env API_KEY=` + both + `"}]}}` + "\n" +
				`{"type":"response_item","payload":{"type":"function_call","name":"` + both + `","call_id":"` + both + `","arguments":"path=/synthetic/` + both + ` API_KEY=` + both + `"}}` + "\n" +
				`{"type":"response_item","payload":{"type":"function_call_output","output":"result/error ` + both + `"}}` + "\n"

			switch scenario {
			case "source_rejected":
				body = strings.Replace(body, nativeID, canaries[0], 1)
			case "bad_json":
				body += `{"A":"` + canaries[0] + `","B":"` + canaries[1] + `",` + "\n"
			case "bad_schema":
				body += `{"type":"response_item","payload":["` + canaries[0] + `","` + canaries[1] + `"]}` + "\n"
			case "depth_limit":
				body += strings.Repeat("[", 34) + `"` + canaries[0] + canaries[1] + `"` + strings.Repeat("]", 34) + "\n"
			case "line_limit":
				body += strings.Repeat("x", 64<<20) + canaries[0] + canaries[1] + "\n"
			}
			if err := os.WriteFile(source, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			metadata := &rcaFlowMetadata{rec: domain.SessionRecord{ID: "selected", ProjectID: "agent-orchestrator", Kind: domain.KindWorker, Harness: domain.HarnessCodex, Mode: domain.SessionModeChat}}
			metadata.rec.Metadata.ProviderConversationID = nativeID
			metadata.rec.Metadata.ControllerGeneration = "controller"
			metadata.rec.Metadata.AgentSettingsLaunchID = "launch"
			metadata.off = scenario == "off"
			if scenario == "window_limit" {
				if err := os.Truncate(source, (1024<<20)+128); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "read_failure" {
				if err := os.Chmod(source, 0000); err != nil {
					t.Fatal(err)
				}
				if f, err := os.Open(source); err == nil {
					_ = f.Close()
					t.Skip("read permissions not enforced")
				}
			}
			if scenario == "background_panic" {
				metadata.beforeList = func(context.Context) { panic(both) }
			}
			entered := make(chan struct{})
			if scenario == "cancel_after_read" {
				var calls atomic.Int32
				metadata.beforeList = func(ctx context.Context) {
					if calls.Add(1) == 3 {
						close(entered)
						<-ctx.Done()
					}
				}
			}
			svcCtx, cancelSvc := context.WithCancel(t.Context())
			defer cancelSvc()
			events := usagesvc.NewEventsReader(metadata)
			data := t.TempDir()
			if fault {
				if err := os.MkdirAll(filepath.Join(data, "rca", "state.next"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			svc := rcasvc.New(svcCtx, data, metadata, events, usagesvc.SourceRoots{CodexSessions: root, CodexArchived: filepath.Join(home, ".codex", "archived_sessions"), AccountCodexSessions: filepath.Join(home, ".ao", "data", "agent-accounts", "history", "codex", "sessions"), AccountCodexArchived: filepath.Join(home, ".ao", "data", "agent-accounts", "history", "codex", "archived_sessions")})
			defer svc.Close()
			var logs bytes.Buffer
			log := slog.New(slog.NewTextHandler(&logs, nil))
			sink := &captureSink{}
			httpCaptures, panicCaptures := 0, 0
			router := NewRouterWithControl(config.Config{}, log, nil, APIDeps{RCA: svc, Telemetry: sink}, ControlDeps{})
			outer := chi.NewRouter()
			outer.Use(requestLoggerWithCapture(log, sink, func(context.Context, error, map[string]string, string) { httpCaptures++ }))
			outer.Use(recoverTelemetryWithCapture(log, sink, func(context.Context, any, string, map[string]string, string) { panicCaptures++ }))
			outer.Get("/control", func(w http.ResponseWriter, r *http.Request) {
				envelope.WriteError(w, r, errors.New("connected control"))
			})
			outer.Get("/panic-control", func(http.ResponseWriter, *http.Request) { panic("connected panic control") })
			outer.Mount("/", router)
			server := httptest.NewServer(outer)
			defer server.Close()
			var responses bytes.Buffer
			assertSafe := func(b []byte) {
				t.Helper()
				for _, canary := range append(canaries, nativeID, "private_tool", "A_SYNTHETIC_LEDGER_KEY", "B_SYNTHETIC_MODEL", home) {
					hash := sha256.Sum256([]byte(canary))
					variants := []string{canary, base64.StdEncoding.EncodeToString([]byte(canary)), base64.RawURLEncoding.EncodeToString([]byte(canary)), hex.EncodeToString([]byte(canary)), hex.EncodeToString(hash[:]), url.QueryEscape(canary)}
					if len(canary) > 16 {
						variants = append(variants, canary[:16], canary[len(canary)-16:])
					}
					for _, v := range variants {
						if bytes.Contains(b, []byte(v)) {
							t.Fatal("new output exposed source or recoverable representation")
						}
					}
				}
			}
			query := func(suffix string) domain.RCAComposition {
				t.Helper()
				response, err := server.Client().Get(server.URL + "/api/v1/usage/sessions/selected/composition" + suffix)
				if err != nil {
					t.Fatal(err)
				}
				b, err := io.ReadAll(response.Body)
				response.Body.Close()
				if err != nil || response.StatusCode != http.StatusOK {
					t.Fatal("composition transport failed")
				}

				responses.Write(b)
				assertSafe(b)
				var out domain.RCAComposition
				if json.Unmarshal(b, &out) != nil {
					t.Fatal("invalid composition")
				}
				return out
			}
			out := query("?analyze=true")
			if scenario == "cancel_after_read" {
				select {
				case <-entered:
					cancelSvc()
				case <-time.After(10 * time.Second):
					t.Fatal("reader never reached cancellation fence")
				}
			}
			deadline := time.Now().Add(30 * time.Second)
			for out.Status == "running" && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
				out = query("")
			}
			if out.Status == "running" || (fault && (out.Status != "unavailable" || out.Reason != "storage_write_failed")) || (scenario == "normal" && out.Recorded == nil) {
				t.Fatalf("unexpected analysis: %s %s", out.Status, out.Reason)
			}
			switch scenario {
			case "off":
				if out.Recorded != nil || out.Reason != "disabled" {
					t.Fatal("off admitted analysis")
				}
			case "read_failure":
				if out.Recorded != nil || out.Reason != "source_read_failed" {
					t.Fatal("read fault not exercised")
				}
			case "background_panic":
				if out.Recorded != nil || out.Reason != "analysis_failed" {
					t.Fatal("background panic not isolated")
				}
			case "cancel_after_read":
				if out.Recorded != nil {
					t.Fatal("canceled reader published results")
				}
			case "bad_json", "bad_schema", "depth_limit", "line_limit", "window_limit", "source_rejected":
				if out.Recorded == nil || len(out.Recorded.Snapshots) != 1 || out.Recorded.Snapshots[0].Status == "ready" {
					t.Fatal("reader fault not exercised")
				}
				expected := map[string]string{"source_rejected": "source_identity_mismatch", "bad_json": "invalid_record", "bad_schema": "missing_key", "depth_limit": "parse_limit", "line_limit": "record_limit", "window_limit": "window_head_gap"}[scenario]
				found := false
				for _, reason := range out.Recorded.Snapshots[0].Reasons {
					if reason == expected {
						found = true
					}
				}
				if !found {
					t.Fatal("expected reader fault reason missing")
				}

			}
			if scenario == "storage_corrupt" {
				p := filepath.Join(data, "rca", "state.json")
				b, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				assertSafe(b)
				b = append(b[:len(b)-1], []byte(`,"unexpected":"`+canaries[0]+canaries[1]+`"}`)...)
				if err := os.WriteFile(p, b, 0600); err != nil {
					t.Fatal(err)
				}
				out = query("")
				if out.Recorded != nil || out.Reason != "storage_read_failed" {
					t.Fatal("corrupt storage exposed")
				}
			}
			if scenario == "origin_rejected" || scenario == "bad_control_json" || scenario == "bad_control_schema" || scenario == "service_5xx" {
				target := "/api/v1/usage/sessions/selected/composition"
				method := http.MethodGet
				input := ""
				want := http.StatusForbidden
				switch scenario {
				case "bad_control_json":
					method = http.MethodPut
					target = "/api/v1/sessions/selected/rca/override"
					input = `{"enabled":` + canaries[0] + canaries[1]
					want = 400
				case "bad_control_schema":
					method = http.MethodPut
					target = "/api/v1/sessions/selected/rca/override"
					input = `{"enabled":true,"raw":"` + canaries[0] + canaries[1] + `"}`
					want = 400
				case "service_5xx":
					metadata.fail = true
					want = 500
				}
				req, err := http.NewRequest(method, server.URL+target, strings.NewReader(input))
				if err != nil {
					t.Fatal(err)
				}
				if scenario == "origin_rejected" {
					req.Header.Set("Origin", "https://"+canaries[0]+canaries[1]+".invalid")
				}
				resp, err := server.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				b, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				if err != nil || resp.StatusCode != want {
					t.Fatal("error boundary not exercised")
				}
				responses.Write(b)
				assertSafe(b)
				metadata.fail = false
			}

			if len(out.Canonical.Events) != 1 || *out.Canonical.Events[0].InputTokens != 42 {
				t.Fatal("canonical query affected")
			}
			page, err := events.Events(t.Context(), "selected", 0, nil, 200)
			if err != nil || page.Events[0].SourceEventKey != "A_SYNTHETIC_LEDGER_KEY" {
				t.Fatal("canonical ledger changed")
			}
			// A healthy native writer can complete another synthetic tool/turn even
			// when RCA's own staging write fails. No provider/model is invoked.
			if err := os.Chmod(source, 0600); err != nil {
				t.Fatal(err)
			}
			f, err := os.OpenFile(source, os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				t.Fatal(err)
			}
			_, err = f.WriteString("{}\n")
			closeErr := f.Close()
			if err != nil || closeErr != nil {
				t.Fatal("native control writer affected")
			}
			if err := filepath.WalkDir(data, func(p string, d os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if scenario == "storage_corrupt" && p == filepath.Join(data, "rca", "state.json") {
					return nil
				}
				if !d.Type().IsRegular() {
					return nil
				}
				b, err := os.ReadFile(p)
				if err != nil {
					return err
				}
				assertSafe(b)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			assertSafe(responses.Bytes())

			// Close all handlers before reading the fake exits (including under race).
			server.Close()
			svc.Close()
			if logs.Len() != 0 || len(sink.events) != 0 || httpCaptures != 0 || panicCaptures != 0 {
				t.Fatal("RCA reached diagnostics")
			}
			positive := httptest.NewRecorder()
			outer.ServeHTTP(positive, httptest.NewRequest(http.MethodGet, "/control", nil))
			outer.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/panic-control", nil))
			if logs.Len() == 0 || len(sink.events) == 0 || httpCaptures == 0 || panicCaptures == 0 || !strings.Contains(logs.String(), "connected control") {
				t.Fatal("diagnostic positive control disconnected")
			}
		})
	}
}
