package controllers_test

import (
	"context"
	"encoding/json"
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
	"github.com/aoagents/agent-orchestrator/backend/internal/mobilebridge"
	sessionsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/session"
)

type privateTransitionService struct {
	*fakeInterfaceTransitionSessionService
}

func (f privateTransitionService) InterfaceTransitionStatus(context.Context, domain.SessionID) (sessionsvc.InterfaceTransitionStatus, error) {
	return sessionsvc.InterfaceTransitionStatus{Supported: true, TargetMode: domain.SessionModeChat, ReasonCode: "TARGET_STOP_UNCONFIRMED", Reason: f.transition.ErrorDetail, Transition: &f.transition}, nil
}

func TestLANTransitionAccountProjection(t *testing.T) {
	tr := domain.SessionInterfaceTransition{ID: "privacy-move", SessionID: "ao-1", SourceMode: domain.SessionModeTUI, TargetMode: domain.SessionModeChat, Phase: domain.SessionInterfaceTransitionRecovery, SourceAccountID: "private-account-A-canary", TargetAccountID: "private-account-B-canary", ErrorCode: "TARGET_STOP_UNCONFIRMED", ErrorDetail: "fake-audit-credential-canary /fake/audit/private/profile"}
	svc := privateTransitionService{&fakeInterfaceTransitionSessionService{fakeSessionService: newFakeSessionService(), transition: tr}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	router := httpd.NewRouterWithControl(config.Config{}, logger, nil, httpd.APIDeps{Sessions: svc}, httpd.ControlDeps{})
	local := httptest.NewServer(router)
	t.Cleanup(local.Close)
	lan := httpd.NewMobileLAN(router, "fake-host", 0, logger, nil)
	lan.SetPasswordHash(mobilebridge.HashPassword("fake-audit-password"))
	port, err := lan.StartLoopback(0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := lan.Stop(context.Background()); err != nil {
			t.Error(err)
		}
	})
	for _, tc := range []struct {
		name, method, path, body string
		status                   int
	}{
		{"status", http.MethodGet, "/api/v1/sessions/ao-1/interface-transition", "", http.StatusOK},
		{"start", http.MethodPost, "/api/v1/sessions/ao-1/interface-transition", `{"targetMode":"chat","policy":"drain"}`, http.StatusAccepted},
		{"acknowledgement", http.MethodPut, "/api/v1/sessions/ao-1/interface-transition/privacy-move/notice-acknowledgement", "", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, remote := range []bool{true, false} {
				base := local.URL
				if remote {
					base = fmt.Sprintf("http://127.0.0.1:%d", port)
				}
				req, err := http.NewRequest(tc.method, base+tc.path, strings.NewReader(tc.body))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Content-Type", "application/json")
				if remote {
					req.Header.Set("Authorization", "Bearer fake-audit-password")
					req.Host = "127.0.0.1"
				}
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				body, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				if err != nil {
					t.Fatal(err)
				}
				if resp.StatusCode != tc.status {
					t.Fatalf("remote=%t status=%d body=%s", remote, resp.StatusCode, body)
				}
				for _, marker := range []string{tr.SourceAccountID, tr.TargetAccountID, "fake-audit-credential-canary", "/fake/audit/private/profile"} {
					if strings.Contains(string(body), marker) == remote {
						t.Errorf("remote=%t privacy/local-preservation failed for %s", remote, marker)
					}
				}
				var wire map[string]json.RawMessage
				if err := json.Unmarshal(body, &wire); err != nil {
					t.Fatal(err)
				}
				var got map[string]json.RawMessage
				if err := json.Unmarshal(wire["transition"], &got); err != nil {
					t.Fatal(err)
				}
				if string(got["id"]) != `"privacy-move"` || string(got["phase"]) != `"recovery_required"` || string(got["errorCode"]) != `"TARGET_STOP_UNCONFIRMED"` {
					t.Fatalf("projection lost useful switch status: %s", body)
				}
				if remote && (got["sourceAccountId"] != nil || got["targetAccountId"] != nil || got["errorDetail"] == nil) {
					t.Fatalf("unsafe or empty remote projection: %s", body)
				}
			}
		})
	}
	if svc.transition != tr {
		t.Fatal("remote projection mutated the shared transition")
	}
}
