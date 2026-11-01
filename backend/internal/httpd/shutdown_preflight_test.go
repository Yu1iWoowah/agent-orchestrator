package httpd

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
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
