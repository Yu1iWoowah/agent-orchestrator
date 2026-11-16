package httpd

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/mobilebridge"
)

func TestLANAccountBoundaryWithPasswordAndDeviceCredentials(t *testing.T) {
	devices, err := mobilebridge.LoadAccessDevices(mobilebridge.AccessDevicesPath(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	_, deviceToken, err := devices.Issue("fixture phone", "test", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusNoContent)
	})
	lan := NewMobileLAN(inner, "account-boundary", 0, nil, nil)
	lan.SetPasswordHash(mobilebridge.HashPassword("fixture-password"))
	lan.UseDeviceCredentials(devices, mobilebridge.NewPairingCodes(nil))
	for _, credential := range []struct{ name, token string }{
		{"none", ""}, {"password", "fixture-password"}, {"device", deviceToken},
	} {
		for _, host := range []string{"paired-host", "127.0.0.1"} {
			for _, route := range []struct{ method, path string }{
				{http.MethodGet, "/api/v1/agents/accounts"},
				{http.MethodHead, "/api/v1/agents/accounts/"},
				{http.MethodPost, "/api/v1/agents/accounts/login"},
				{http.MethodPost, "/api/v1/agents/accounts/acct-1/logout"},
				{http.MethodDelete, "/api/v1/agents/accounts/acct-1"},
				{http.MethodPut, "/api/v1/agents/accounts/sessions/worker-1"},
				{http.MethodPatch, "/api/v1/agents/accounts/acct-1"},
				{http.MethodGet, "/api/v1/agents/accounts/login-terminal"},
				{http.MethodGet, "/api/v1/agents/accounts/operations/op-1/pty"},
				{http.MethodOptions, "/api/v1/agents/accounts/operations/op-1"},
			} {
				t.Run(credential.name+"/"+host+"/"+route.method+route.path, func(t *testing.T) {
					req := httptest.NewRequest(route.method, route.path, nil)
					req.Host = host
					if credential.token != "" {
						req.Header.Set("Authorization", "Bearer "+credential.token)
					}
					response := httptest.NewRecorder()
					lan.handler.ServeHTTP(response, req)
					if response.Code != http.StatusNotFound || calls != 0 {
						t.Fatalf("account boundary: status=%d calls=%d", response.Code, calls)
					}
				})
			}
		}
	}
	// A valid device still reaches the independent public model route.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/codex/models", nil)
	req.Header.Set("Authorization", "Bearer "+deviceToken)
	response := httptest.NewRecorder()
	lan.handler.ServeHTTP(response, req)
	if response.Code != http.StatusNoContent || calls != 1 {
		t.Fatalf("device model route: status=%d calls=%d", response.Code, calls)
	}
	// The shared handler remains callable directly on the primary listener.
	response = httptest.NewRecorder()
	inner.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/agents/accounts", nil))
	if response.Code != http.StatusNoContent || calls != 2 {
		t.Fatalf("primary account route: status=%d calls=%d", response.Code, calls)
	}
}
