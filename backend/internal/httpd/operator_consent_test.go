package httpd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
)

func TestOperatorConsentReadinessAndNoPublicMint(t *testing.T) {
	r := NewRouterWithControl(config.Config{DataDir: t.TempDir(), AllowedOrigins: []string{"http://127.0.0.1:3001"}}, discardLogger(), nil, APIDeps{}, ControlDeps{})
	for _, caller := range []string{"anonymous", "agent", "browser", "lan_bearer", "actor_user", "confirm", "local_operator_unverified"} {
		t.Run(caller, func(t *testing.T) {
			request := func(method, path string) *httptest.ResponseRecorder {
				req := httptest.NewRequest(method, "http://127.0.0.1:3001"+path, strings.NewReader(`{"actor":"user","confirm":true,"authority":"local_operator_unverified","requestRef":"known-ref"}`))
				switch caller {
				case "agent":
					req.Header.Set("X-AO-Session-ID", "agent-1")
				case "browser":
					req.Header.Set("Origin", "http://127.0.0.1:3001")
				case "lan_bearer":
					req.Header.Set("Authorization", "Bearer arbitrary")
				case "actor_user":
					req.Header.Set("X-AO-Actor", "user")
				case "confirm":
					req.Header.Set("X-AO-Confirm", "true")
				case "local_operator_unverified":
					req.Header.Set("X-AO-Authority", "local_operator_unverified")
				}
				out := httptest.NewRecorder()
				r.ServeHTTP(out, req)
				return out
			}
			out := request(http.MethodGet, "/api/v1/operator-consent/readiness")
			if out.Code != http.StatusOK {
				t.Fatalf("readiness %d: %s", out.Code, out.Body)
			}
			var got controllers.OperatorConsentReadinessResponse
			if err := json.Unmarshal(out.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.AuthorityGo || got.NativeIssuerDelivered || len(got.PurposeSet) != 0 || got.IssuerEpoch != "" || got.QualificationRef != "" || got.ExpiresAt != nil {
				t.Fatalf("unsafe readiness %+v", got)
			}
			for _, path := range []string{"/api/v1/operator-consent/confirm", "/api/v1/operator-consent/mint", "/api/v1/operator-consent/prepare", "/api/v1/operator-consent/consume", "/internal/operator-consent/confirm", "/api/v1/operator-consent/readiness"} {
				out = request(http.MethodPost, path)
				if out.Code != http.StatusNotFound && out.Code != http.StatusMethodNotAllowed && out.Code != http.StatusForbidden {
					t.Fatalf("public mint %s returned %d", path, out.Code)
				}
				if strings.Contains(out.Body.String(), "consentRef") || strings.Contains(out.Body.String(), "challenge") {
					t.Fatalf("material in response %s", out.Body)
				}
			}
		})
	}
}
