package controllers_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd"
	"github.com/aoagents/agent-orchestrator/backend/internal/mobilebridge"
)

type retryAccountHTTPService struct {
	*fakeSessionService
	calls     atomic.Int32
	confirmed bool
}

func (s *retryAccountHTTPService) RetryAccountRecovery(_ context.Context, id domain.SessionID) (bool, error) {
	if id != "fake-session" {
		panic("wrong session recovery")
	}
	s.calls.Add(1)
	return s.confirmed, nil
}
func TestAccountRecoveryHTTPKeepsLANReadOnlyAndReportsConfirmation(t *testing.T) {
	for _, confirmed := range []bool{false, true} {
		t.Run(fmt.Sprint(confirmed), func(t *testing.T) {
			svc := &retryAccountHTTPService{fakeSessionService: newFakeSessionService(), confirmed: confirmed}
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			router := httpd.NewRouterWithControl(config.Config{}, logger, nil, httpd.APIDeps{Sessions: svc}, httpd.ControlDeps{})
			local := httptest.NewServer(router)
			defer local.Close()
			lan := httpd.NewMobileLAN(router, "fake-host", 0, logger, nil)
			lan.SetPasswordHash(mobilebridge.HashPassword("fake-password"))
			port, err := lan.StartLoopback(0)
			if err != nil {
				t.Fatal(err)
			}
			defer lan.Stop(context.Background())
			req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/api/v1/sessions/fake-session/account-recovery", port), nil)
			req.Header.Set("Authorization", "Bearer fake-password")
			req.Host = "127.0.0.1"
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(res.Body)
			res.Body.Close()
			if res.StatusCode != http.StatusForbidden || svc.calls.Load() != 0 {
				t.Fatalf("LAN mutated local recovery: %d %s calls=%d", res.StatusCode, body, svc.calls.Load())
			}
			res, err = http.Post(local.URL+"/api/v1/sessions/fake-session/account-recovery", "application/json", nil)
			if err != nil {
				t.Fatal(err)
			}
			body, _ = io.ReadAll(res.Body)
			res.Body.Close()
			want := fmt.Sprintf(`{"confirmed":%t}`, confirmed)
			if res.StatusCode != http.StatusOK || string(body) != want+"\n" || svc.calls.Load() != 1 {
				t.Fatalf("local confirmation: %d %s calls=%d", res.StatusCode, body, svc.calls.Load())
			}
		})
	}
}
