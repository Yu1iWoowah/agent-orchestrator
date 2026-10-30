package cli

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

type settingsReconnectTransport struct {
	base  http.RoundTripper
	polls atomic.Int32
}

func (s *settingsReconnectTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.HasSuffix(r.URL.Path, "/operations/op-reconnect") && s.polls.Add(1) == 1 {
		return nil, errors.New("injected daemon disconnect")
	}
	return s.base.RoundTrip(r)
}

func TestSessionSettingsReadsQualifiedStateWithoutProjection(t *testing.T) {
	cfg := setConfigEnv(t)
	want := `{"sessionId":"demo-1","revision":7,"settings":{"fastMode":"off"},"pending":{"id":"op-1","phase":"pending","target":{"fastMode":"on"}},"effective":{"state":"unknown","reason":"not_provided"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/sessions/demo-1/agent-settings" {
			_, _ = io.WriteString(w, want)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	writeRunFileFor(t, cfg, srv)
	out, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "session", "settings", "demo-1", "--json")
	var got, expected any
	if err != nil || json.Unmarshal([]byte(out), &got) != nil || json.Unmarshal([]byte(want), &expected) != nil || !reflect.DeepEqual(got, expected) {
		t.Fatalf("settings must preserve committed, pending and unknown: output=%q error=%v", out, err)
	}
}

func TestSessionSetWaitReloadsDaemonRunFileAfterAcceptance(t *testing.T) {
	cfg := setConfigEnv(t)
	var patches, replacementReads atomic.Int32
	replacement := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/sessions/demo-1/agent-settings/operations/op-moved" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/sessions/demo-1/agent-settings/operations/op-moved" {
			t.Errorf("unexpected replacement daemon request: %s %s", r.Method, r.URL.Path)
		}
		replacementReads.Add(1)
		_, _ = io.WriteString(w, `{"operation":{"id":"op-moved","sessionId":"demo-1","phase":"applied","version":2}}`)
	}))
	t.Cleanup(replacement.Close)
	original := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/sessions/demo-1/agent-settings" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodPatch {
			t.Errorf("poll used old daemon: %s", r.Method)
		}
		patches.Add(1)
		writeRunFileFor(t, cfg, replacement)
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{"operation":{"id":"op-moved","sessionId":"demo-1","phase":"pending","version":1}}`)
	}))
	t.Cleanup(original.Close)
	writeRunFileFor(t, cfg, original)
	out, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "session", "set", "demo-1", "--fast", "off", "--if-revision", "4", "--wait", "--wait-timeout", "2s", "--json")
	if err != nil || patches.Load() != 1 || replacementReads.Load() != 1 || !strings.Contains(out, "applied") {
		t.Fatalf("wait did not reconnect without mutation: patches=%d reads=%d output=%q error=%v", patches.Load(), replacementReads.Load(), out, err)
	}
}

func TestSessionSetSendsRevisionCheckedTypedReset(t *testing.T) {
	for _, explicitRevision := range []bool{false, true} {
		t.Run(map[bool]string{false: "read revision", true: "explicit revision"}[explicitRevision], func(t *testing.T) {
			cfg := setConfigEnv(t)
			var gets, patches atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/sessions/demo-1/agent-settings" {
					w.WriteHeader(http.StatusNoContent)
					return
				}
				if r.Method == http.MethodGet {
					gets.Add(1)
					_, _ = io.WriteString(w, `{"sessionId":"demo-1","revision":7}`)
					return
				}
				patches.Add(1)
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				want := map[string]any{"expectedRevision": float64(7), "applyMode": "restart", "stopTurn": true, "settings": map[string]any{"model": nil, "effort": "high", "fastMode": "off"}}
				if r.Method != http.MethodPatch || !reflect.DeepEqual(body, want) || r.Header.Get("Idempotency-Key") == "" {
					t.Errorf("unexpected settings mutation: method=%s body=%v key=%q", r.Method, body, r.Header.Get("Idempotency-Key"))
				}
				w.WriteHeader(http.StatusAccepted)
				_, _ = io.WriteString(w, `{"operation":{"id":"op-1","sessionId":"demo-1","phase":"pending","version":1}}`)
			}))
			t.Cleanup(srv.Close)
			writeRunFileFor(t, cfg, srv)
			args := []string{"session", "set", "demo-1", "--model", "default", "--effort", "high", "--fast", "off", "--restart", "--stop-turn", "--json"}
			if explicitRevision {
				args = append(args, "--if-revision", "7")
			}
			out, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, args...)
			if err != nil || !strings.Contains(out, "op-1") || !strings.Contains(out, "pending") || patches.Load() != 1 || gets.Load() != map[bool]int32{true: 0, false: 1}[explicitRevision] {
				t.Fatalf("mutation output=%q error=%v gets=%d patches=%d", out, err, gets.Load(), patches.Load())
			}
		})
	}
}

func TestSessionSetPreservesConflictEnvelopeWithoutRetry(t *testing.T) {
	cfg := setConfigEnv(t)
	var patches atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			patches.Add(1)
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"message":"settings revision changed","code":"AGENT_SETTINGS_CONFLICT","requestId":"cas-request"}`)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	writeRunFileFor(t, cfg, srv)
	_, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "session", "set", "demo-1", "--fast", "off", "--if-revision", "7")
	if ExitCode(err) != 1 || !strings.Contains(err.Error(), "AGENT_SETTINGS_CONFLICT") || !strings.Contains(err.Error(), "cas-request") || patches.Load() != 1 {
		t.Fatalf("CAS must not silently retry: error=%v patches=%d", err, patches.Load())
	}
}

func TestSessionSetWaitTimeoutPreservesAcceptedOperation(t *testing.T) {
	cfg := setConfigEnv(t)
	var patches, cancels atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			patches.Add(1)
		}
		if strings.HasSuffix(r.URL.Path, "/cancel") {
			cancels.Add(1)
		}
		if strings.Contains(r.URL.Path, "agent-settings") {
			_, _ = io.WriteString(w, `{"operation":{"id":"op-timeout","sessionId":"demo-1","phase":"pending","version":1}}`)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	writeRunFileFor(t, cfg, srv)
	_, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "session", "set", "demo-1", "--fast", "off", "--if-revision", "7", "--wait", "--wait-timeout", "20ms")
	if ExitCode(err) != 1 || !strings.Contains(err.Error(), "op-timeout") || !strings.Contains(err.Error(), "timeout") || patches.Load() != 1 || cancels.Load() != 0 {
		t.Fatalf("timeout must retain reconnect handle: error=%v patches=%d cancels=%d", err, patches.Load(), cancels.Load())
	}
}

func TestSessionSettingsRejectsInvalidUsageBeforeHTTP(t *testing.T) {
	setConfigEnv(t)
	t.Setenv("AO_SESSION_ID", "")
	for _, args := range [][]string{
		{"settings"}, {"set"}, {"set", "demo-1"},
		{"set", "demo-1", "--fast", "true"},
		{"set", "demo-1", "--fast", ""},
		{"set", "demo-1", "--fast", "on", "--if-revision", "-1"},
		{"set", "demo-1", "--fast", "off", "--stop-turn"},
		{"set", "demo-1", "--fast", "off", "--wait-timeout", "1s"},
		{"set", "demo-1", "--fast", "off", "--wait", "--wait-timeout", "0s"},
		{"set", "demo-1", "--fast", "off", "--account", "managed"},
		{"settings-operation"}, {"settings-operation", "op-1"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			_, _, err := executeCLI(t, Deps{}, append([]string{"session"}, args...)...)
			if ExitCode(err) != 2 {
				t.Fatalf("invalid usage must exit 2 before reading daemon state: %v", err)
			}
		})
	}
}

func TestSessionSetWaitReconnectsWithoutReissuingMutation(t *testing.T) {
	cfg := setConfigEnv(t)
	var patches atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		phase := "applied"
		if r.Method == http.MethodPatch {
			patches.Add(1)
			phase = "pending"
		}
		if strings.Contains(r.URL.Path, "agent-settings") {
			_ = json.NewEncoder(w).Encode(map[string]any{"operation": map[string]any{"id": "op-reconnect", "sessionId": "demo-1", "phase": phase, "version": 1}})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	writeRunFileFor(t, cfg, srv)
	transport := &settingsReconnectTransport{base: srv.Client().Transport}
	deps := Deps{ProcessAlive: func(int) bool { return true }, HTTPClient: &http.Client{Transport: transport}}
	out, _, err := executeCLI(t, deps, "session", "set", "demo-1", "--fast", "off", "--if-revision", "7", "--wait", "--wait-timeout", "2s", "--json")
	if err != nil || !strings.Contains(out, "applied") || patches.Load() != 1 || transport.polls.Load() != 2 {
		t.Fatalf("reconnect must poll the same operation, never resend PATCH: output=%q error=%v patches=%d polls=%d", out, err, patches.Load(), transport.polls.Load())
	}
	out, _, err = executeCLI(t, deps, "session", "settings-operation", "op-reconnect", "--session", "demo-1", "--json")
	if err != nil || !strings.Contains(out, "op-reconnect") || !strings.Contains(out, "applied") {
		t.Fatalf("operation reconnect output=%q error=%v", out, err)
	}
}

func TestSessionSetReportsImmediateOperationFailure(t *testing.T) {
	for _, phase := range []string{"failed", "cancelled", "recovery_required"} {
		t.Run(phase, func(t *testing.T) {
			cfg := setConfigEnv(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPatch {
					_ = json.NewEncoder(w).Encode(map[string]any{"operation": map[string]any{"id": "op-failure", "sessionId": "demo-1", "phase": phase, "errorCode": "AGENT_SETTINGS_FAILED", "requestId": "failed-request"}})
					return
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			t.Cleanup(srv.Close)
			writeRunFileFor(t, cfg, srv)
			_, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "session", "set", "demo-1", "--fast", "off", "--if-revision", "7")
			if ExitCode(err) != 1 || !strings.Contains(err.Error(), "AGENT_SETTINGS_FAILED") || !strings.Contains(err.Error(), "failed-request") || !strings.Contains(err.Error(), phase) {
				t.Fatalf("failed operation must exit 1 and preserve diagnostic: %v", err)
			}
		})
	}
}
