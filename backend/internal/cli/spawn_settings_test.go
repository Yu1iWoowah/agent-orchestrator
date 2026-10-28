package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestSpawnSettingsNegotiatesBeforeSendingNewWire(t *testing.T) {
	for _, tc := range []struct {
		name, support string
		flags         []string
		want          map[string]any
		reject        bool
	}{
		{"fast on", `{"agentSettingsVersion":1}`, []string{"--fast", "on"}, map[string]any{"fastMode": "on"}, false},
		{"resets", `{"agentSettingsVersion":1}`, []string{"--model", "default", "--effort", "default", "--fast", "default"}, map[string]any{"model": nil, "effort": nil, "fastMode": "default"}, false},
		{"old daemon", "404", []string{"--fast", "off"}, nil, true},
		{"old daemon reset", "404", []string{"--model", "default"}, nil, true},
		{"missing declaration", `{}`, []string{"--fast", "on"}, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := setConfigEnv(t)
			posted, negotiated := false, false
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/api/v1/sessions/spawn-capabilities":
					negotiated = true
					if tc.support == "404" {
						http.NotFound(w, r)
						return
					}
					_, _ = io.WriteString(w, tc.support)
				case r.Method == http.MethodPost && r.URL.Path == "/api/v1/sessions":
					posted = true
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					if !negotiated || !reflect.DeepEqual(body["agentSettings"], tc.want) {
						t.Errorf("new wire=%+v negotiated=%v", body, negotiated)
					}
					if _, ok := body["model"]; ok {
						t.Error("reset leaked legacy model")
					}
					if _, ok := body["effort"]; ok {
						t.Error("reset leaked legacy effort")
					}
					_, _ = io.WriteString(w, `{"session":{"id":"standalone-1","status":"idle"}}`)
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(srv.Close)
			writeRunFileFor(t, cfg, srv)
			args := append([]string{"spawn", "--standalone", "--agent", "codex", "--name", "test", "--skip-agent-check"}, tc.flags...)
			_, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, args...)
			if tc.reject {
				if err == nil || posted || !negotiated {
					t.Fatalf("old daemon err=%v posted=%v negotiated=%v", err, posted, negotiated)
				}
				return
			}
			if err != nil || !posted {
				t.Fatalf("spawn err=%v posted=%v", err, posted)
			}
		})
	}
}

func TestSpawnReportsInheritedEffortReset(t *testing.T) {
	cfg := setConfigEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/sessions" {
			_, _ = io.WriteString(w, `{"session":{"id":"standalone-1","status":"idle","spawnAgentSettings":{"validation":"catalog","effortCleared":true}}}`)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	writeRunFileFor(t, cfg, srv)
	out, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "spawn", "--standalone", "--agent", "codex", "--name", "test", "--skip-agent-check", "--model", "native")
	if err != nil || !strings.Contains(out, "inherited effort reset to provider default") {
		t.Fatalf("out=%q err=%v", out, err)
	}
}
