package appdiagnostics

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"

	diag "github.com/aoagents/agent-orchestrator/backend/internal/service/appdiagnostics"
)

func TestPrivateHandoffSeparatesBrowserAndDiagnostics(t *testing.T) {
	for _, tt := range []struct {
		raw, browser, app string
		bad               bool
	}{{"browser\n" + `{"version":1,"browserToken":"browser","appDiagnosticsToken":"abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG"}` + "\n", "browser", "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG", false}, {"legacy-browser\n", "legacy-browser", "", false}, {`{"version":1,"browserToken":"browser","appDiagnosticsToken":"abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG"}` + "\n", "browser", "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG", false}, {`{"version":2,"browserToken":"browser","appDiagnosticsToken":"abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG"}`, "", "", true}, {`{"version":1,"browserToken":"same","appDiagnosticsToken":"same"}`, "", "", true}, {strings.Repeat("x", 2049), "", "", true}} {
		got, err := ReadCredentials(strings.NewReader(tt.raw))
		if tt.bad {
			if err == nil {
				t.Fatal("invalid handoff accepted")
			}
			continue
		}
		if err != nil || got.BrowserToken != tt.browser || got.AppToken != tt.app {
			t.Fatalf("unexpected handoff result: %v", err)
		}
	}
}
func connect(t *testing.T, b *Broker, token string) (net.Conn, *json.Decoder, *json.Encoder) {
	t.Helper()
	server, client := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	t.Cleanup(func() { _ = client.Close() })
	go b.serveConn(ctx, server)
	dec := json.NewDecoder(client)
	enc := json.NewEncoder(client)
	if err := enc.Encode(map[string]any{"type": "hello", "version": 1, "token": token, "instanceId": "app-1"}); err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	return client, dec, enc
}
func TestBrokerRejectsBrowserToken(t *testing.T) {
	b := New("diagnostics-token")
	c, d, _ := connect(t, b, "browser-token")
	var out map[string]any
	_ = d.Decode(&out)
	if b.State().Ready || out["type"] != "hello-rejected" {
		t.Fatal("browser token gained app diagnostic access")
	}
	_ = c.Close()
}
func TestBrokerCaptureFencesReplyAndCancels(t *testing.T) {
	b := New("diagnostics-token")
	c, d, e := connect(t, b, "diagnostics-token")
	var hello map[string]any
	if err := d.Decode(&hello); err != nil {
		t.Fatal(err)
	}
	if hello["type"] != "hello-ok" {
		t.Fatal(hello)
	}
	if err := e.Encode(map[string]any{"type": "ready", "windows": []diag.Window{{ID: "window-1", Generation: 1}}}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for !b.State().Ready && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := b.Capture(ctx, diag.CaptureRequest{WindowID: "window-1"}); done <- err }()
	var command map[string]any
	if err := d.Decode(&command); err != nil {
		t.Fatal(err)
	}
	if command["type"] != "snapshot" || command["requestId"] == "" {
		t.Fatal(command)
	}
	cancel()
	var cancellation map[string]any
	if err := d.Decode(&cancellation); err != nil {
		t.Fatal(err)
	}
	if cancellation["type"] != "cancel" || cancellation["requestId"] != command["requestId"] {
		t.Fatal(cancellation)
	}
	if err := <-done; err == nil {
		t.Fatal("cancelled capture succeeded")
	}
	_ = c.Close()
	deadline = time.Now().Add(time.Second)
	for b.State().Ready && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if b.State().Ready {
		t.Fatal("disconnect retained ready state")
	}
}

func TestBrokerRoundTripAndReplayClosesConnection(t *testing.T) {
	b := New("diagnostics-token")
	c, d, e := connect(t, b, "diagnostics-token")
	var hello map[string]any
	if err := d.Decode(&hello); err != nil {
		t.Fatal(err)
	}
	if err := e.Encode(map[string]any{"type": "ready", "windows": []diag.Window{{ID: "window-1", Generation: 1}}}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for !b.State().Ready && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	done := make(chan error, 1)
	go func() {
		_, err := b.Capture(context.Background(), diag.CaptureRequest{WindowID: "window-1"})
		done <- err
	}()
	var command map[string]any
	if err := d.Decode(&command); err != nil {
		t.Fatal(err)
	}
	reply := map[string]any{"type": "result", "requestId": command["requestId"], "snapshot": diag.Snapshot{SchemaVersion: 1, InstanceID: "app-1", WindowID: "window-1", Generation: 1, CapturedAt: time.Now(), Completeness: "partial"}}
	if err := e.Encode(reply); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := e.Encode(reply); err != nil {
		t.Fatal(err)
	}
	var ignored map[string]any
	if err := d.Decode(&ignored); err == nil {
		t.Fatal("replay left connection usable")
	}
	_ = c.Close()
}

func TestBrokerRejectsFutureProtocol(t *testing.T) {
	b := New("diagnostics-token")
	server, client := net.Pipe()
	defer client.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.serveConn(ctx, server)
	if err := json.NewEncoder(client).Encode(map[string]any{"type": "hello", "version": 2, "token": "diagnostics-token", "instanceId": "app-1"}); err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	var out map[string]any
	if err := json.NewDecoder(client).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out["type"] != "hello-rejected" {
		t.Fatal("unsupported runtime protocol accepted")
	}
}

type grantSessions struct{}

func (grantSessions) Get(_ context.Context, id domain.SessionID) (domain.Session, error) {
	return domain.Session{SessionRecord: domain.SessionRecord{ID: id, ProjectID: "project", Kind: domain.KindOrchestrator, Metadata: domain.SessionMetadata{ControllerGeneration: "launch-1"}}}, nil
}
func TestAuthenticatedRuntimeCannotGrantBeforeCollectorReady(t *testing.T) {
	b := New("diagnostics-token")
	dir := t.TempDir()
	service, err := diag.New(dir, grantSessions{}, b, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	b.BindAuthority(service)
	_, decoder, encoder := connect(t, b, "diagnostics-token")
	var hello map[string]any
	if err := decoder.Decode(&hello); err != nil {
		t.Fatal(err)
	}
	if err := encoder.Encode(map[string]any{"type": "grant", "requestId": "request-1", "sessionId": "orchestrator-1"}); err != nil {
		t.Fatal(err)
	}
	var reply map[string]any
	if err := decoder.Decode(&reply); err != nil {
		t.Fatal(err)
	}
	if reply["code"] != "APP_DIAGNOSTICS_UNSUPPORTED" {
		t.Fatalf("runtime enabled without collector: %v", reply)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "diagnostics", "grants"))
	if err != nil || len(entries) != 0 {
		t.Fatal("credential published before collector ready")
	}
}

func TestBrokerPreservesOnlyFixedCollectorFailures(t *testing.T) {
	for _, tt := range []struct {
		code   string
		status int
	}{{"APP_DIAGNOSTICS_PAYLOAD_TOO_LARGE", 413}, {"APP_DIAGNOSTICS_INVALID_PAYLOAD", 502}, {"APP_RENDERER_CHANGED", 409}, {"APP_SNAPSHOT_TIMEOUT", 504}, {"canary-secret /private/path", 503}} {
		t.Run(tt.code, func(t *testing.T) {
			b := New("diagnostics-token")
			c, d, e := connect(t, b, "diagnostics-token")
			defer c.Close()
			var hello frame
			if err := d.Decode(&hello); err != nil {
				t.Fatal(err)
			}
			if err := e.Encode(frame{Type: "ready", Windows: []domain.AppDiagnosticsWindow{{ID: "window-1", Generation: 1}}}); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(time.Second)
			for !b.State().Ready && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			done := make(chan error, 1)
			go func() {
				_, err := b.Capture(context.Background(), domain.AppDiagnosticsCaptureRequest{WindowID: "window-1"})
				done <- err
			}()
			var command frame
			if err := d.Decode(&command); err != nil {
				t.Fatal(err)
			}
			if err := e.Encode(frame{Type: "result", RequestID: command.RequestID, Code: tt.code}); err != nil {
				t.Fatal(err)
			}
			err := <-done
			var typed *domain.AppDiagnosticsError
			if !errors.As(err, &typed) || typed.Status != tt.status || strings.Contains(err.Error(), "canary-secret") {
				t.Fatalf("unsafe or collapsed failure: %v", err)
			}
		})
	}
}
func TestBrokerRejectsUnknownCollectorContent(t *testing.T) {
	raw, err := os.ReadFile("../../../test/fixtures/app-diagnostics-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var snapshot map[string]any
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatal(err)
	}
	collector := snapshot["collector"].(map[string]any)
	for _, section := range []string{"route", "shell", "cache", "components", "geometry", "errors"} {
		data := collector[section].(map[string]any)
		data["payload"] = "canary-secret /private/path"
		frameBytes, _ := json.Marshal(map[string]any{"type": "result", "snapshot": snapshot})
		var decoded frame
		if decode(frameBytes, &decoded) == nil {
			t.Fatalf("unknown content accepted in %s", section)
		}
		delete(data, "payload")
	}
}
