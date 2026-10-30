package controllers_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	sessionsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/session"
	sessionmanager "github.com/aoagents/agent-orchestrator/backend/internal/session_manager"
)

type leaseRejectedManager struct {
	*sessionmanager.Manager
	err error
}

func (m leaseRejectedManager) Spawn(context.Context, ports.SpawnConfig) (domain.SessionRecord, int, int, error) {
	return domain.SessionRecord{}, 0, 0, m.err
}

func (m leaseRejectedManager) ResumeAgentWithMode(context.Context, domain.SessionID) (sessionmanager.RestoreResult, error) {
	return sessionmanager.RestoreResult{}, m.err
}

func TestSessionRoutesReturnRetryable409ForRejectedAccountLease(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code string
	}{
		{ports.ErrAgentAccountInUse, "AGENT_ACCOUNT_IN_USE"},
		{ports.ErrAgentAccountOperationBusy, "AGENT_ACCOUNT_OPERATION_BUSY"},
	} {
		for _, operation := range []string{"spawn", "resume"} {
			t.Run(operation+"/"+tc.code, func(t *testing.T) {
				// Exercise the real session service and HTTP error boundary. Only
				// the manager's denied lease is faked; no runtime can start.
				svc := sessionsvc.NewWithDeps(sessionsvc.Deps{Manager: leaseRejectedManager{err: fmt.Errorf("private-provider-canary: %w", tc.err)}})
				server := httptest.NewServer(httpd.NewRouterWithControl(config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, httpd.APIDeps{Sessions: svc}, httpd.ControlDeps{}))
				t.Cleanup(server.Close)
				path, request := "/api/v1/sessions", `{"kind":"worker","harness":"claude-code","mode":"tui","prompt":"fake"}`
				if operation == "resume" {
					path, request = "/api/v1/sessions/fake-1/resume-agent", ""
				}
				body, status, _ := doRequest(t, server, http.MethodPost, path, request)
				assertErrorCode(t, body, status, http.StatusConflict, tc.code)
				if !strings.Contains(string(body), `"retryable":true`) || strings.Contains(string(body), "canary") || strings.Contains(string(body), "cleanup is scheduled") {
					t.Fatalf("incorrect lease error: %s", body)
				}
			})
		}
	}
}
