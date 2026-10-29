package controllers_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestSessionsAPISpawnSettingsPreservesResetAndFingerprint(t *testing.T) {
	svc := newFakeSessionService()
	srv := newSessionTestServer(t, svc)
	hashes := map[string]bool{}
	for _, patch := range []string{`{}`, `{"model":null}`, `{"effort":null}`, `{"fastMode":"default"}`, `{"fastMode":"off"}`} {
		body, status, _ := doRequest(t, srv, "POST", "/api/v1/sessions", `{"clientRequestId":"settings-key","harness":"codex","agentSettings":`+patch+`}`)
		if status != http.StatusCreated {
			t.Fatalf("patch=%s status=%d body=%s", patch, status, body)
		}
		if svc.lastSpawn.AgentSettings == nil {
			t.Fatal("typed patch lost")
		}
		raw, err := json.Marshal(svc.lastSpawn.AgentSettings)
		if err != nil || string(raw) != patch {
			t.Fatalf("patch=%s wire=%s err=%v", patch, raw, err)
		}
		hash := svc.lastSpawn.ClientRequestHash
		if hash == "" || hashes[hash] {
			t.Fatalf("reset/inherit collision: patch=%s hash=%s", patch, hash)
		}
		hashes[hash] = true
	}
}

func TestSessionsAPISpawnRejectsMixedSettings(t *testing.T) {
	svc := newFakeSessionService()
	srv := newSessionTestServer(t, svc)
	for _, raw := range []string{
		`{"model":"legacy","agentSettings":{"model":null}}`,
		`{"model":"","agentSettings":{"model":"native"}}`,
		`{"Model":"","agentSettings":{"model":"native"}}`,
		`{"effort":"","agentSettings":{"effort":null}}`,
		`{"agentSettings":{"fastMode":"turbo"}}`,
	} {
		body, status, _ := doRequest(t, srv, "POST", "/api/v1/sessions", raw)
		if status != http.StatusBadRequest {
			t.Fatalf("request=%s status=%d body=%s", raw, status, body)
		}
	}
}

func TestSessionsAPIDeclaresSpawnSettingsSupport(t *testing.T) {
	_, status, _ := doRequest(t, newSessionTestServer(t, newFakeSessionService()), "GET", "/api/v1/sessions/spawn-capabilities", "")
	if status != http.StatusOK {
		t.Fatalf("capability declaration status=%d", status)
	}
}
