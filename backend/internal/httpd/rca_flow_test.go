package httpd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
type rcaFlowMetadata struct{ rec domain.SessionRecord }

func (m *rcaFlowMetadata) GetProject(context.Context, string) (domain.ProjectRecord, bool, error) {
	on := true
	return domain.ProjectRecord{ID: "agent-orchestrator", Config: domain.ProjectConfig{RCA: domain.RCAConfig{Enabled: &on}}}, true, nil
}
func (m *rcaFlowMetadata) GetSession(_ context.Context, id domain.SessionID) (domain.SessionRecord, bool, error) {
	return m.rec, id == m.rec.ID, nil
}
func (m *rcaFlowMetadata) ListSessions(context.Context, domain.ProjectID) ([]domain.SessionRecord, error) {
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
func (*rcaFlowMetadata) ListUsageEventsPage(context.Context, domain.SessionID, int64, int64, int64) ([]domain.UsageLedgerEvent, error) {
	n := int64(42)
	return []domain.UsageLedgerEvent{{ID: 1, BindingID: 2, UsageSourceID: 3, ModelUsageEvent: domain.ModelUsageEvent{SourceEventKey: "A_SYNTHETIC_LEDGER_KEY", ModelID: "B_SYNTHETIC_MODEL", Tokens: domain.UsageTokenMetrics{InputTokens: &n}}}}, nil
}

func TestRCAActualHTTPFlowCanariesAndCanonicalControl(t *testing.T) {
	for _, fault := range []bool{false, true} {
		t.Run(map[bool]string{false: "normal", true: "storage_failure"}[fault], func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			nativeID := "11111111-1111-4111-8111-111111111111"
			root := filepath.Join(home, ".codex", "sessions")
			if err := os.MkdirAll(root, 0700); err != nil {
				t.Fatal(err)
			}
			canaries := []string{"A_SYNTHETIC_CREDENTIAL_CANARY", "B_SYNTHETIC_CONTENT_CANARY"}
			source := filepath.Join(root, "rollout-lab-"+nativeID+".jsonl")
			body := `{"type":"session_meta","payload":{"id":"` + nativeID + `","cli_version":"0.167.0","source":"cli","base_instructions":{"text":"` + canaries[0] + `"},"dynamic_tools":[{"name":"private_tool"}]}}` + "\n" + `{"type":"response_item","payload":{"type":"function_call_output","output":"` + canaries[1] + `"}}` + "\n"
			if err := os.WriteFile(source, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			metadata := &rcaFlowMetadata{rec: domain.SessionRecord{ID: "selected", ProjectID: "agent-orchestrator", Kind: domain.KindWorker, Harness: domain.HarnessCodex, Mode: domain.SessionModeChat}}
			metadata.rec.Metadata.ProviderConversationID = nativeID
			metadata.rec.Metadata.ControllerGeneration = "controller"
			metadata.rec.Metadata.AgentSettingsLaunchID = "launch"
			events := usagesvc.NewEventsReader(metadata)
			data := t.TempDir()
			if fault {
				if err := os.MkdirAll(filepath.Join(data, "rca", "state.next"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			svc := rcasvc.New(t.Context(), data, metadata, events, usagesvc.SourceRoots{CodexSessions: root, CodexArchived: filepath.Join(home, ".codex", "archived_sessions"), AccountCodexSessions: filepath.Join(home, ".ao", "data", "agent-accounts", "history", "codex", "sessions"), AccountCodexArchived: filepath.Join(home, ".ao", "data", "agent-accounts", "history", "codex", "archived_sessions")})
			defer svc.Close()
			var logs bytes.Buffer
			log := slog.New(slog.NewTextHandler(&logs, nil))
			sink := &captureSink{}
			captures := 0
			router := NewRouterWithControl(config.Config{}, log, nil, APIDeps{RCA: svc, Telemetry: sink}, ControlDeps{})
			outer := chi.NewRouter()
			outer.Use(requestLoggerWithCapture(log, sink, func(context.Context, error, map[string]string, string) { captures++ }))
			outer.Use(recoverTelemetryWithCapture(log, sink, func(context.Context, any, string, map[string]string, string) { captures++ }))
			outer.Get("/control", func(w http.ResponseWriter, r *http.Request) {
				envelope.WriteError(w, r, errors.New("connected control"))
			})
			outer.Mount("/", router)
			server := httptest.NewServer(outer)
			defer server.Close()
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
				for _, canary := range append(canaries, nativeID, "private_tool", "A_SYNTHETIC_LEDGER_KEY", "B_SYNTHETIC_MODEL", home) {
					if bytes.Contains(b, []byte(canary)) {
						t.Fatal("HTTP exposed source")
					}
				}
				var out domain.RCAComposition
				if json.Unmarshal(b, &out) != nil {
					t.Fatal("invalid composition")
				}
				return out
			}
			out := query("?analyze=true")
			deadline := time.Now().Add(3 * time.Second)
			for out.Status == "running" && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
				out = query("")
			}
			if out.Status == "running" || (fault && (out.Status != "unavailable" || out.Reason != "storage_write_failed")) || (!fault && out.Recorded == nil) {
				t.Fatalf("unexpected analysis: %s %s", out.Status, out.Reason)
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
			f, err := os.OpenFile(source, os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				t.Fatal(err)
			}
			_, err = f.WriteString("{}\n")
			closeErr := f.Close()
			if err != nil || closeErr != nil {
				t.Fatal("native control writer affected")
			}
			if !fault {
				b, err := os.ReadFile(filepath.Join(data, "rca", "state.json"))
				if err != nil {
					t.Fatal(err)
				}
				for _, canary := range append(canaries, nativeID, "private_tool", home) {
					if bytes.Contains(b, []byte(canary)) {
						t.Fatal("numeric store retained source")
					}
				}
			}
			// Close all handlers before reading the fake exits (including under race).
			server.Close()
			if logs.Len() != 0 || len(sink.events) != 0 || captures != 0 {
				t.Fatal("RCA reached diagnostics")
			}
			positive := httptest.NewRecorder()
			outer.ServeHTTP(positive, httptest.NewRequest(http.MethodGet, "/control", nil))
			if logs.Len() == 0 || len(sink.events) == 0 || captures == 0 || !strings.Contains(logs.String(), "connected control") {
				t.Fatal("diagnostic positive control disconnected")
			}
		})
	}
}
