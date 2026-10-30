package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReviewTriggerFastIsReviewerOwned(t *testing.T) {
	cfg := setConfigEnv(t)
	var request struct {
		AgentConfig struct {
			Model    string `json:"model"`
			Effort   string `json:"effort"`
			FastMode string `json:"fastMode"`
		} `json:"agentConfig"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/reviews/trigger") {
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			_, _ = io.WriteString(w, `{"created":true}`)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	writeRunFileFor(t, cfg, srv)
	_, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "review", "trigger", "worker-1", "--model", "review-model", "--effort", "high", "--fast", "off")
	if err != nil || request.AgentConfig.Model != "review-model" || request.AgentConfig.Effort != "high" || request.AgentConfig.FastMode != "off" {
		t.Fatalf("request=%+v error=%v", request, err)
	}
}

func TestReviewSetUsesStableOwnerAndDoesNotRetryConflict(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(map[bool]string{false: "pinned", true: "conflict"}[conflict], func(t *testing.T) {
			cfg := setConfigEnv(t)
			patches := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPatch {
					w.WriteHeader(http.StatusNoContent)
					return
				}
				patches++
				if r.URL.Path != "/api/v1/reviews/stable-review/agent-settings" || r.Header.Get("Idempotency-Key") == "" {
					t.Errorf("wrong reviewer owner/key: %s %s", r.Method, r.URL.Path)
				}
				var body struct {
					ExpectedRevision int64          `json:"expectedRevision"`
					Settings         map[string]any `json:"settings"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body.ExpectedRevision != 9 || body.Settings["model"] != nil || body.Settings["fastMode"] != "off" {
					t.Errorf("wrong CAS/reset: %+v", body)
				}
				if conflict {
					w.WriteHeader(http.StatusConflict)
					_, _ = io.WriteString(w, `{"code":"AGENT_SETTINGS_CONFLICT","message":"active pass","requestId":"review-cas"}`)
					return
				}
				_, _ = io.WriteString(w, `{"id":"pin","reviewId":"stable-review","phase":"pinned","applyMode":"next-pass","revision":10}`)
			}))
			t.Cleanup(srv.Close)
			writeRunFileFor(t, cfg, srv)
			out, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "review", "set", "stable-review", "--model", "default", "--fast", "off", "--if-revision", "9", "--json")
			if patches != 1 {
				t.Fatalf("patches=%d", patches)
			}
			if conflict {
				if ExitCode(err) != 1 || !strings.Contains(err.Error(), "review-cas") || !strings.Contains(err.Error(), "AGENT_SETTINGS_CONFLICT") {
					t.Fatalf("lost envelope: %v", err)
				}
			} else if err != nil || !strings.Contains(out, `"phase": "pinned"`) {
				t.Fatalf("pin=%s error=%v", out, err)
			}
		})
	}
}

func TestReviewSettingsUsageFailsBeforeDaemon(t *testing.T) {
	for _, args := range [][]string{{"review", "set"}, {"review", "set", "owner"}, {"review", "set", "owner", "--fast", "turbo"}, {"review", "set", "owner", "--model", "m", "--if-revision", "-1"}, {"review", "trigger", "worker", "--fast", "turbo"}} {
		_, _, err := executeCLI(t, Deps{}, args...)
		if ExitCode(err) != 2 {
			t.Fatalf("%v: %v", args, err)
		}
	}
}
