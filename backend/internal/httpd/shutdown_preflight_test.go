package httpd

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestShutdownPreflightRejectsBeforeSuccessResponse(t *testing.T) {
	requested := false
	router := NewRouterWithControl(config.Config{}, discardLogger(), nil, APIDeps{}, ControlDeps{RequestShutdown: func() { requested = true }, ShutdownPreflight: func(context.Context) error { return errors.New("private-provider-text-must-not-leak") }})
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/shutdown", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "shutdown_not_settled") || strings.Contains(response.Body.String(), "private-provider-text") || requested {
		t.Fatalf("unsafe shutdown response: %d %s", response.Code, response.Body.String())
	}
}

func TestShutdownReportsHistoricalUnknownSides(t *testing.T) {
	router := NewRouterWithControl(config.Config{}, discardLogger(), nil, APIDeps{}, ControlDeps{RequestShutdown: func() {}, ShutdownPreflight: func(context.Context) error { return nil }, ShutdownPreservedSides: func() []string { return []string{"historical-side"} }})
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/shutdown", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	if response.Code != http.StatusAccepted || !strings.Contains(response.Body.String(), `"historical_unknown_sides":["historical-side"]`) || !strings.Contains(response.Body.String(), `"new_unknown_count":0`) {
		t.Fatalf("%d %s", response.Code, response.Body.String())
	}
}

func TestShutdownReportsProtectedSideReasonAndEntry(t *testing.T) {
	router := NewRouterWithControl(config.Config{}, discardLogger(), nil, APIDeps{}, ControlDeps{RequestShutdown: func() { t.Error("shutdown requested") }, ShutdownPreflight: func(context.Context) error {
		return &ports.ChatShutdownSideError{SideID: "side-1", Reason: "scope_host_alive"}
	}})
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/shutdown", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"sideId":"side-1"`) || !strings.Contains(response.Body.String(), `"reason":"scope_host_alive"`) || !strings.Contains(response.Body.String(), `"entry":"protected_side_chats"`) {
		t.Fatalf("%d %s", response.Code, response.Body.String())
	}
}
