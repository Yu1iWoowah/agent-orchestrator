package cli

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestA10GuardianCLIValidationAndPurgeConfirmation(t *testing.T) {
	for _, args := range [][]string{{"guardian-observability"}, {"guardian-observability", "s", "--since", "yesterday"}, {"guardian-observability", "s", "--since", "2026-01-01T00:00:00+08:00"}, {"guardian-observability", "s", "--limit", "1"}, {"guardian-observability", "s", "--details", "--limit", "501"}, {"guardian-observability", "s", "--details", "--limit", "0"}, {"guardian-observability", "purge", "s"}, {"guardian-observability", "s", "--since", "2026-01-02T00:00:00Z", "--until", "2026-01-01T00:00:00Z"}} {
		t.Run(strings.Join(args, "/"), func(t *testing.T) {
			setConfigEnv(t)
			_, _, err := executeCLI(t, Deps{In: strings.NewReader("")}, append([]string{"session"}, args...)...)
			var usage usageError
			if !errors.As(err, &usage) {
				t.Fatalf("expected usage error, got %v", err)
			}
		})
	}
}
func TestA8A10GuardianCLIWhitelistRequestAndErrors(t *testing.T) {
	cfg := setConfigEnv(t)
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			_, _ = io.WriteString(w, `{"status":"ok"}`)
			return
		}
		if r.URL.Path == "/internal/telemetry/cli-invoked" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		calls = append(calls, requestLogEntry(r))
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "missing") {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"code":"SESSION_NOT_FOUND","message":"Session not found","requestId":"request-123"}`)
			return
		}
		if r.Method == http.MethodDelete {
			_, _ = io.WriteString(w, `{"purged":true}`)
			return
		}
		_, _ = io.WriteString(w, `{"schema_version":1,"enabled":true,"coverage":"partial","requests":3,"totals":{"input_tokens":100,"unknown_fields":{"cache_write":3}},"command":"SENSITIVE_CONTENT_SENTINEL","details":[{"kind":"request","response_id":"response","rationale":"SENSITIVE_CONTENT_SENTINEL"}]}`)
	}))
	defer srv.Close()
	writeRunFileFor(t, cfg, srv)
	out, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "session", "guardian-observability", "s", "--details", "--limit", "3", "--since", "2026-01-01T00:00:00Z", "--until", "2026-01-02T00:00:00Z", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "SENSITIVE_CONTENT_SENTINEL") || !strings.Contains(out, `"requests": 3`) || !strings.Contains(out, `"response_id": "response"`) {
		t.Fatalf("bad DTO: %s", out)
	}
	if len(calls) != 1 || !strings.Contains(calls[0], "details=true") || !strings.Contains(calls[0], "limit=3") || !strings.Contains(calls[0], "since=2026-01-01T00%3A00%3A00Z") {
		t.Fatalf("bad request: %v", calls)
	}
	_, _, err = executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "session", "guardian-observability", "missing", "--json")
	if err == nil || !strings.Contains(err.Error(), "request-123") || !strings.Contains(err.Error(), "SESSION_NOT_FOUND") {
		t.Fatalf("error envelope lost: %v", err)
	}
	out, _, err = executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "session", "guardian-observability", "purge", "s", "--yes")
	if err != nil || !strings.Contains(out, `"purged": true`) || calls[len(calls)-1] != "DELETE /api/v1/sessions/s/guardian-observability" {
		t.Fatalf("purge failed: %s %v", out, err)
	}
}
