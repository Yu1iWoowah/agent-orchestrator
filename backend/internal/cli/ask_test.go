package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const askCLIPayload = `{"schemaVersion":1,"title":"Deploy target","explanation":"A deployment needs your decision","questions":[{"key":"target","header":"Target","question":"Where?","options":[{"id":"stage","label":"Staging","description":"Internal users"},{"id":"prod","label":"Production","description":"Public users"}]}]}`
const askCLIAnswer = `{"questionId":"ask-1","expectedVersion":1,"clientAnswerId":"answer-1","answers":[{"questionKey":"target","selectedOptionIds":["stage"]}]}`

func askCLIFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "payload.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAskCLICommandsUseOnlyDaemonRoutes(t *testing.T) {
	payloadFile := askCLIFile(t, askCLIPayload)
	answerWithNotes := strings.Replace(askCLIAnswer, `"selectedOptionIds":["stage"]`, `"selectedOptionIds":["stage"],"notes":"  選 A，但 PR3 先不要做\n原文  "`, 1)
	answerFile := askCLIFile(t, strings.TrimSuffix(answerWithNotes, "}")+`,"notes":"總補充"}`)
	for _, tc := range []struct {
		name, method, path, bodyContains string
		args                             []string
	}{
		{"create", "POST", "/api/v1/sessions/worker-1/questions", `"clientKey":"deploy"`, []string{"ask", "create", "--key", "deploy", "--audience", "user", "--kind", "question", "--payload-file", payloadFile, "--json"}},
		{"list", "GET", "/api/v1/sessions/worker-1/questions?beforeSequence=22&limit=2&state=history", "", []string{"ask", "list", "--state", "history", "--before-sequence", "22", "--limit", "2", "--json"}},
		{"get", "GET", "/api/v1/sessions/worker-1/questions/ask-1", "", []string{"ask", "get", "ask-1", "--json"}},
		{"answer", "POST", "/api/v1/sessions/worker-1/questions/ask-1/answer", `"clientAnswerId":"answer-1"`, []string{"ask", "answer", "ask-1", "--payload-file", answerFile, "--json"}},
		{"read", "POST", "/api/v1/sessions/worker-1/questions/ask-1/read", `"expectedVersion":1`, []string{"ask", "read", "ask-1", "--expected-version", "1"}},
		{"cancel", "POST", "/api/v1/sessions/worker-1/questions/ask-1/cancel", `"actor":"agent"`, []string{"ask", "cancel", "ask-1", "--expected-version", "1", "--reason", "No longer needed", "--json"}},
		{"revise", "POST", "/api/v1/sessions/worker-1/questions/ask-1/revise", `"expectedVersion":1`, []string{"ask", "revise", "ask-1", "--expected-version", "1", "--payload-file", payloadFile, "--json"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := setConfigEnv(t)
			t.Setenv("AO_SESSION_ID", "worker-1")
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/internal/telemetry/cli-invoked" {
					w.WriteHeader(http.StatusNoContent)
					return
				}
				calls++
				raw, _ := io.ReadAll(r.Body)
				if r.Method != tc.method || r.URL.RequestURI() != tc.path || !strings.Contains(string(raw), tc.bodyContains) {
					t.Errorf("request %s %s %s", r.Method, r.URL.RequestURI(), raw)
				}
				if tc.name == "answer" && (!strings.Contains(string(raw), `"notes":"  選 A，但 PR3 先不要做\n原文  "`) || !strings.Contains(string(raw), `"notes":"總補充"`)) {
					t.Errorf("notes lost: %s", raw)
				}
				if tc.name == "read" {
					w.WriteHeader(http.StatusNoContent)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"ask-1","state":"open","version":1,"payload":`+askCLIPayload+`,"items":[],"open":0,"unread":0,"history":0,"clientAnswerId":"answer-1","deliveryState":"queued"}`)
			}))
			defer server.Close()
			writeRunFileFor(t, cfg, server)
			stdout, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, tc.args...)
			if err != nil || calls != 1 {
				t.Fatalf("err=%v calls=%d stdout=%s", err, calls, stdout)
			}
			if tc.name != "read" && !json.Valid([]byte(stdout)) {
				t.Fatalf("invalid JSON stdout %s", stdout)
			}
		})
	}
}

func TestAskReportForwardUsesOnlyItsOwningCapability(t *testing.T) {
	for _, tc := range []struct {
		name, actor, capability, target string
		allowed                         bool
	}{
		{"owner", "owner", "owner-proof", "owner", true},
		{"forged session", "worker", "worker-proof", "owner", false},
		{"missing capability", "owner", "", "owner", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := setConfigEnv(t)
			t.Setenv("AO_SESSION_ID", tc.actor)
			t.Setenv("AO_BROWSER_CAPABILITY", tc.capability)
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/internal/telemetry/cli-invoked" {
					w.WriteHeader(http.StatusNoContent)
					return
				}
				calls++
				if r.Header.Get(browserCapabilityHeader) != "owner-proof" {
					t.Error("forwarding omitted the owning capability")
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"question","version":1}`)
			}))
			defer server.Close()
			writeRunFileFor(t, cfg, server)
			stdout, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "ask", "create", "--session", tc.target, "--source-report", "report", "--key", "forward", "--payload-file", askCLIFile(t, askCLIPayload), "--json")
			if tc.allowed && (err != nil || calls != 1) || !tc.allowed && (err == nil || calls != 0) {
				t.Fatalf("allowed=%v calls=%d err=%v", tc.allowed, calls, err)
			}
			if strings.Contains(stdout, tc.capability) && tc.capability != "" {
				t.Fatal("capability leaked to stdout")
			}
		})
	}
}

func TestAskCLIRejectsInvalidInputBeforeDaemon(t *testing.T) {
	t.Setenv("AO_SESSION_ID", "worker-1")
	good := askCLIFile(t, askCLIPayload)
	bad := askCLIFile(t, `{"schemaVersion":1}`)
	large := askCLIFile(t, strings.Repeat(" ", 128*1024+1))
	for _, args := range [][]string{
		{"ask", "create"},
		{"ask", "create", "--key", "deploy", "--payload-file", bad},
		{"ask", "create", "--key", "deploy", "--payload-file", large},
		{"ask", "create", "--key", "bad key", "--payload-file", good},
		{"ask", "create", "--key", "deploy", "--audience", "orchestrator", "--payload-file", good},
		{"ask", "read", "ask-1"},
		{"ask", "cancel", "ask-1", "--expected-version", "1"},
		{"ask", "revise", "ask-1", "--expected-version", "0", "--payload-file", good},
		{"ask", "answer", "different-id", "--payload-file", askCLIFile(t, askCLIAnswer)},
		{"ask", "list", "--limit", "101"},
		{"ask", "list", "--state", "unknown"},
	} {
		_, _, err := executeCLI(t, Deps{}, args...)
		if err == nil || ExitCode(err) != 2 {
			t.Fatalf("args=%v err=%v exit=%d", args, err, ExitCode(err))
		}
	}
}

func TestAskCLIConflictPreservesRequestIDAndUnknownSubmissionIsNotSuccess(t *testing.T) {
	cfg := setConfigEnv(t)
	t.Setenv("AO_SESSION_ID", "worker-1")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"message":"Reload question","code":"QUESTION_CHANGED","requestId":"req-question-7"}`)
	}))
	defer server.Close()
	writeRunFileFor(t, cfg, server)
	out, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "ask", "answer", "ask-1", "--payload-file", askCLIFile(t, askCLIAnswer))
	if err == nil || ExitCode(err) != 1 || !strings.Contains(err.Error(), "QUESTION_CHANGED") || !strings.Contains(err.Error(), "req-question-7") || out != "" {
		t.Fatalf("conflict err=%v out=%s", err, out)
	}
}

func TestAskCLIRejectsOversizedEnvelopeBeforeDaemon(t *testing.T) {
	t.Setenv("AO_SESSION_ID", "worker-1")
	for _, action := range []string{"create", "revise"} {
		t.Run(action, func(t *testing.T) {
			var payload map[string]any
			if err := json.Unmarshal([]byte(askCLIPayload), &payload); err != nil {
				t.Fatal(err)
			}
			prompts := make([]any, 4)
			for i := range prompts {
				options := make([]any, 4)
				for j := range options {
					options[j] = map[string]any{"id": fmt.Sprintf("o%d", j), "label": fmt.Sprintf("Choice %d", j), "description": "Reason", "preview": map[string]any{"format": "text", "content": strings.Repeat("p", 8192)}}
				}
				prompts[i] = map[string]any{"key": fmt.Sprintf("q%d", i), "header": "Target", "question": "Where?", "options": options}
			}
			payload["questions"] = prompts
			raw, _ := json.Marshal(payload)
			first := prompts[0].(map[string]any)["options"].([]any)[0].(map[string]any)["preview"].(map[string]any)
			first["content"] = strings.Repeat("p", 8192-(len(raw)-(128*1024-1)))
			raw, _ = json.Marshal(payload)
			body := string(raw)
			args := []string{"ask", action}
			if action == "create" {
				args = append(args, "--key", "deploy")
			} else {
				args = append(args, "ask-1", "--expected-version", "1")
			}
			args = append(args, "--payload-file", askCLIFile(t, body))
			_, _, err := executeCLI(t, Deps{}, args...)
			if err == nil || ExitCode(err) != 2 || !strings.Contains(err.Error(), "envelope") {
				t.Fatalf("must reject the whole envelope before connecting: %v", err)
			}
		})
	}
}

func TestAskProjectCLIUsesExplicitAudienceAndCursor(t *testing.T) {
	cfg := setConfigEnv(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal/telemetry/cli-invoked" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.URL.RequestURI() != "/api/v1/projects/project/attention?audience=orchestrator&cursor=next&limit=2" {
			t.Errorf("unexpected request: %s", r.URL.RequestURI())
		}
		_, _ = io.WriteString(w, `{"items":[],"open":5,"unread":3}`)
	}))
	defer server.Close()
	writeRunFileFor(t, cfg, server)
	out, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "ask", "project", "--project", "project", "--audience", "orchestrator", "--cursor", "next", "--limit", "2", "--json")
	if err != nil || !strings.Contains(out, `"open": 5`) {
		t.Fatalf("out=%s err=%v", out, err)
	}
}

func TestAskCLIReadsSavedNotes(t *testing.T) {
	for _, action := range []string{"get", "list"} {
		for _, jsonOutput := range []bool{false, true} {
			if action == "list" && !jsonOutput {
				continue
			}
			t.Run(fmt.Sprintf("%s/json=%t", action, jsonOutput), func(t *testing.T) {
				cfg := setConfigEnv(t)
				t.Setenv("AO_SESSION_ID", "worker-1")
				record := `{"id":"ask-1","state":"answered","version":1,"payload":` + askCLIPayload + `,"answer":{"answers":[{"questionKey":"target","selectedOptionIds":["stage"],"notes":"  選 A，但 PR3 先不要做\n原文  "}],"notes":"總補充"}}`
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/internal/telemetry/cli-invoked" {
						w.WriteHeader(http.StatusNoContent)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					if action == "list" {
						_, _ = io.WriteString(w, `{"items":[`+record+`],"history":1}`)
					} else {
						_, _ = io.WriteString(w, record)
					}
				}))
				defer server.Close()
				writeRunFileFor(t, cfg, server)
				args := []string{"ask", action}
				if action == "get" {
					args = append(args, "ask-1")
				} else {
					args = append(args, "--state", "history")
				}
				if jsonOutput {
					args = append(args, "--json")
				}
				stdout, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, args...)
				if err != nil || !strings.Contains(stdout, `"notes": "總補充"`) || !strings.Contains(stdout, "PR3 先不要做") {
					t.Fatalf("saved notes missing: %s %v", stdout, err)
				}
			})
		}
	}
}
