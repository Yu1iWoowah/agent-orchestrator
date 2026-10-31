package controllers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestReviewSettingsErrorEnvelope(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	for _, tc := range []struct {
		name         string
		err          error
		status       int
		code, reason string
	}{
		{"unsupported", ports.ErrAgentSettingsUnsupported, 400, "AGENT_SETTINGS_UNSUPPORTED", "selection_unsupported"},
		{"unavailable", ports.ErrAgentSettingsCapabilitiesUnavailable, 503, "AGENT_SETTINGS_CAPABILITIES_UNAVAILABLE", "capabilities_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs.Reset()
			req := httptest.NewRequest(http.MethodPost, "/reviews/trigger", nil)
			req = req.WithContext(context.WithValue(req.Context(), middleware.RequestIDKey, "alias-request"))
			req, captured := envelope.WithErrorCapture(req)
			w := httptest.NewRecorder()
			writeReviewError(w, req, fmt.Errorf("token=secret /private/account: %w", tc.err))
			var body envelope.APIError
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if w.Code != tc.status || body.Code != tc.code || body.RequestID != "alias-request" || body.Details["reasonCode"] != tc.reason {
				t.Fatalf("HTTP %d: %s", w.Code, w.Body.String())
			}
			for _, value := range []string{w.Body.String(), fmt.Sprint(captured().Err), logs.String()} {
				if strings.Contains(value, "token=secret") || strings.Contains(value, "/private/account") {
					t.Fatalf("unsafe diagnostics: %s", value)
				}
			}
			if !strings.Contains(logs.String(), `"reasonCode":"`+tc.reason+`"`) || !strings.Contains(logs.String(), `"request_id":"alias-request"`) {
				t.Fatalf("missing safe log diagnostics: %s", logs.String())
			}
			if captured().Err == nil {
				t.Fatal("safe structured diagnostics were not captured")
			}
		})
	}
}
