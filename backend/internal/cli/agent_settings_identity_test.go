package cli

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSettingsClientCarriesIdentityWithoutClaimingLocalHuman(t *testing.T) {
	for _, tc := range []struct{ session, grant string }{{"", ""}, {"worker", ""}, {"orchestrator", "purpose-grant"}} {
		r := httptest.NewRequest(http.MethodPatch, "http://127.0.0.1/api/v1/sessions/worker/agent-settings", nil)
		applySettingsIdentity(r, func(key string) string {
			if key == "AO_SESSION_ID" {
				return tc.session
			}
			if key == "AO_AGENT_SETTINGS_GRANT" {
				return tc.grant
			}
			return ""
		})
		if r.Header.Get("X-AO-Session-ID") != tc.session || r.Header.Get("X-AO-Settings-Grant") != tc.grant {
			t.Fatal("lost caller identity")
		}
	}
}
