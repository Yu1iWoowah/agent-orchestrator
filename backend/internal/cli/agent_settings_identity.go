package cli

import "net/http"

// AO_SESSION_ID is a claim only. The daemon authenticates it using the
// independently injected settings-purpose grant before any managed mutation.
func applySettingsIdentity(req *http.Request, getenv func(string) string) {
	if id := getenv("AO_SESSION_ID"); id != "" {
		req.Header.Set("X-AO-Session-ID", id)
	}
	if grant := getenv("AO_AGENT_SETTINGS_GRANT"); grant != "" {
		req.Header.Set("X-AO-Settings-Grant", grant)
	}
}
