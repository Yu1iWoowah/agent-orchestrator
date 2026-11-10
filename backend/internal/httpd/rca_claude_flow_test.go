package httpd

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	rcasvc "github.com/aoagents/agent-orchestrator/backend/internal/service/rca"
	usagesvc "github.com/aoagents/agent-orchestrator/backend/internal/service/usage"
)

func claudeWireField(field int, data []byte) []byte {
	out := binary.AppendUvarint(nil, uint64(field<<3|2))
	out = binary.AppendUvarint(out, uint64(len(data)))
	return append(out, data...)
}
func claudeWireBody(body string, native string) []byte {
	var record []byte
	for key, value := range map[string]string{"event.name": "api_request_body", "session.id": native, "request_body_id": "synthetic-body-id", "request_id": "synthetic-request-id", "body": body} {
		attr := append(claudeWireField(1, []byte(key)), claudeWireField(2, claudeWireField(1, []byte(value)))...)
		record = append(record, claudeWireField(6, attr)...)
	}
	return claudeWireField(1, claudeWireField(2, claudeWireField(2, record)))
}

type claudeFaultBody struct {
	io.ReadCloser
	cancel          context.CancelFunc
	fail, panicRead bool
}

func (b *claudeFaultBody) Read(p []byte) (int, error) {
	if b.panicRead {
		panic("A_SYNTHETIC_CREDENTIAL_CANARY B_SYNTHETIC_CONTENT_CANARY")
	}
	if b.fail {
		return 0, errors.New("A_SYNTHETIC_CREDENTIAL_CANARY B_SYNTHETIC_CONTENT_CANARY")
	}
	n, e := b.ReadCloser.Read(p)
	if b.cancel != nil {
		b.cancel()
	}
	return n, e
}
func TestRCAClaudeActualHTTPFlowCanariesAndCanonicalControl(t *testing.T) {
	for _, scenario := range []string{"normal", "off", "origin_rejected", "namespace_rejected", "bad_otlp", "bad_json", "bad_schema", "depth_limit", "body_limit", "encoding_rejected", "read_failure", "cancel_after_read", "background_panic", "storage_failure", "service_5xx"} {
		t.Run(scenario, func(t *testing.T) {
			data := t.TempDir()
			native := "11111111-1111-4111-8111-111111111111"
			metadata := &rcaFlowMetadata{rec: domain.SessionRecord{ID: "selected", ProjectID: "agent-orchestrator", Kind: domain.KindOrchestrator, Harness: domain.HarnessClaudeCode, Mode: domain.SessionModeChat}}
			metadata.rec.Metadata.ProviderConversationID = native
			metadata.rec.Metadata.ControllerGeneration = "controller"
			metadata.rec.Metadata.AgentSettingsLaunchID = "native-launch"
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			profile := ports.RCAClaudeRuntime{BinaryPath: "/synthetic/claude", BridgePath: "/synthetic/bridge", OS: "synthetic", BinaryVersion: "fake-1", BridgeVersion: "fake-2", SDKVersion: "fake-3"}
			svc := rcasvc.New(ctx, data, metadata, usagesvc.NewEventsReader(metadata), usagesvc.SourceRoots{}, func(actual ports.RCAClaudeRuntime, role domain.SessionKind) bool {
				return actual == profile && role == domain.KindOrchestrator
			})
			defer svc.Close()
			svc.SetClaudeLaunchEndpoint("http://127.0.0.1:12345/api/v1/rca/otlp/v1/logs")
			if _, err := svc.Get(t.Context(), "selected", domain.RCAQuery{Limit: 200}); err != nil {
				t.Fatal(err)
			}
			svc.InvalidateClaudeLaunch("selected")
			notice := ports.RCAClaudeLaunch{SessionID: "selected", ProjectID: "agent-orchestrator", Kind: domain.KindOrchestrator, DataRoot: data, Generation: "controller"}
			env := svc.PrepareClaudeEnvironment(t.Context(), notice, profile, nil)
			marker := strings.TrimPrefix(env["OTEL_EXPORTER_OTLP_LOGS_HEADERS"], "X-AO-RCA-Capture=")
			if marker == "" {
				t.Fatal("fake capability did not prepare namespace")
			}
			svc.ClaudeLaunchReady(t.Context(), notice, false)
			if scenario == "off" {
				off := false
				if _, err := svc.SetOverride(t.Context(), "selected", &off); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "storage_failure" {
				if err := os.MkdirAll(filepath.Join(data, "rca", "state.next"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "service_5xx" {
				metadata.fail = true
			}
			var logs bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&logs, nil))
			sink := &captureSink{}
			var httpCaptures, panicCaptures atomic.Uint64
			router := NewRouterWithControl(config.Config{AllowedOrigins: []string{"app://renderer"}}, logger, nil, APIDeps{RCA: svc, Telemetry: sink}, ControlDeps{})
			outer := chi.NewRouter()
			outer.Use(requestLoggerWithCapture(logger, sink, func(context.Context, error, map[string]string, string) { httpCaptures.Add(1) }))
			outer.Use(recoverTelemetryWithCapture(logger, sink, func(context.Context, any, string, map[string]string, string) { panicCaptures.Add(1) }))
			outer.Use(func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if isRCARawRequest(r) {
						switch scenario {
						case "read_failure":
							r.Body = &claudeFaultBody{ReadCloser: r.Body, fail: true}
						case "cancel_after_read":
							r.Body = &claudeFaultBody{ReadCloser: r.Body, cancel: cancel}
						case "background_panic":
							r.Body = &claudeFaultBody{ReadCloser: r.Body, panicRead: true}
						}
					}
					next.ServeHTTP(w, r)
				})
			})
			outer.Get("/control", func(w http.ResponseWriter, r *http.Request) {
				envelope.WriteError(w, r, errors.New("connected control"))
			})
			outer.Get("/panic-control", func(http.ResponseWriter, *http.Request) { panic("connected control") })
			outer.Mount("/", router)
			server := httptest.NewServer(outer)
			t.Cleanup(server.Close)
			client := server.Client()
			client.Timeout = 3 * time.Second
			canaries := []string{"A_SYNTHETIC_CREDENTIAL_CANARY", "B_SYNTHETIC_CONTENT_CANARY"}
			body := `{"system":"` + strings.Join(canaries, " ") + `","messages":[{"role":"user","content":"test"}]}`
			switch scenario {
			case "bad_json":
				body = `{"system":"` + strings.Join(canaries, " ")
			case "bad_schema":
				body = `["` + strings.Join(canaries, " ") + `"]`
			case "depth_limit":
				body = `{"system":` + strings.Repeat("[", 33) + `"` + strings.Join(canaries, " ") + `"` + strings.Repeat("]", 33) + `}`
			case "body_limit":
				body = `{"system":"` + strings.Repeat("x", 256<<10) + strings.Join(canaries, " ") + `"}`
			}
			wire := claudeWireBody(body, native)
			if scenario == "bad_otlp" {
				wire = append([]byte{0}, wire...)
			}
			req, err := http.NewRequest(http.MethodPost, server.URL+rcaRawPath, bytes.NewReader(wire))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/x-protobuf")
			req.Header.Set("X-AO-RCA-Capture", marker)
			if scenario == "origin_rejected" {
				req.Header.Set("Origin", "app://renderer")
			}
			if scenario == "namespace_rejected" {
				req.Header.Set("X-AO-RCA-Capture", "unknown")
			}
			if scenario == "encoding_rejected" {
				req.Header.Set("Content-Encoding", "gzip")
			}
			response, err := client.Do(req)
			reset := scenario == "body_limit" && errors.Is(err, syscall.ECONNRESET)
			if err != nil && !reset {
				t.Fatal(err)
			}
			var outputs bytes.Buffer
			var raw []byte
			status := 0
			if response != nil {
				status = response.StatusCode
				raw, _ = io.ReadAll(response.Body)
				_ = response.Body.Close()
			}
			outputs.Write(raw)
			healthy := scenario == "normal" || scenario == "storage_failure" || scenario == "service_5xx"
			if healthy && status != 200 || !healthy && !reset && status < 400 {
				t.Fatalf("status=%d", status)
			}
			if healthy && len(raw) != 0 {
				t.Fatal("successful OTLP ACK is not empty protobuf")
			}
			var composition domain.RCAComposition
			until := time.Now().Add(2 * time.Second)
			for {
				resp, e := client.Get(server.URL + "/api/v1/usage/sessions/selected/composition")
				if e != nil {
					t.Fatal(e)
				}
				raw, _ = io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				outputs.Write(raw)
				if scenario == "service_5xx" {
					if resp.StatusCode != http.StatusInternalServerError {
						t.Fatal("canonical read failure hidden")
					}
					break
				}
				if resp.StatusCode != http.StatusOK || json.Unmarshal(raw, &composition) != nil {
					t.Fatal("query failed")
				}
				if composition.Reason != "writer_busy" {
					break
				}
				if time.Now().After(until) {
					t.Fatal("numeric writer blocked")
				}
				time.Sleep(time.Millisecond)
			}
			if scenario == "normal" && (composition.ClaudeInline == nil || !composition.ClaudeInline.Active || len(composition.ClaudeInline.Attempts) != 1) {
				t.Fatal("real reader/projection flow missing")
			}
			if scenario == "encoding_rejected" || scenario == "bad_otlp" || scenario == "bad_json" || scenario == "bad_schema" || scenario == "depth_limit" || scenario == "body_limit" || scenario == "read_failure" || scenario == "background_panic" || scenario == "storage_failure" {
				if composition.ClaudeInline != nil && composition.ClaudeInline.Active {
					t.Fatal("fault left source active")
				}
			}
			if scenario != "service_5xx" && len(composition.Canonical.Events) != 1 {
				t.Fatal("canonical control affected")
			}
			if httpCaptures.Load() != 0 || panicCaptures.Load() != 0 || len(sink.events) != 0 {
				t.Fatal("RCA reached a diagnostic exit")
			}
			for _, path := range []string{"/control", "/panic-control"} {
				resp, e := client.Get(server.URL + path)
				if e != nil {
					t.Fatal(e)
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}
			server.Close()
			if httpCaptures.Load() == 0 || panicCaptures.Load() == 0 || len(sink.events) == 0 || logs.Len() == 0 {
				t.Fatal("non-RCA diagnostic controls ineffective")
			}
			outputs.Write(logs.Bytes())
			if raw, e := os.ReadFile(filepath.Join(data, "rca", "state.json")); e == nil {
				outputs.Write(raw)
			}
			for _, canary := range append(canaries, native, "synthetic-body-id", "synthetic-request-id", marker) {
				for _, form := range []string{canary, hex.EncodeToString([]byte(canary)), base64.StdEncoding.EncodeToString([]byte(canary))} {
					if bytes.Contains(outputs.Bytes(), []byte(form)) {
						t.Fatal("original/recoverable value escaped RCA")
					}
				}
			}
		})
	}
}
