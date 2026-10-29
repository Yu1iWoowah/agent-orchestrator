package cli

import (
	"encoding/json"
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
	answerFile := askCLIFile(t, askCLIAnswer)
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
