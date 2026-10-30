//go:build !windows

package e2e

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Only the native provider is fake. The daemon, detached ACP host, SQLite,
// history API and CDC/SSE transport are the production implementations.
func TestFakeHarnessBackgroundProvider(t *testing.T) {
	dir := os.Getenv("AO_E2E_BACKGROUND_DIR")
	if dir == "" {
		return
	}
	var mu sync.Mutex
	write := func(frame any) {
		mu.Lock()
		defer mu.Unlock()
		if err := json.NewEncoder(os.Stdout).Encode(frame); err != nil {
			os.Exit(2)
		}
	}
	notify := func(method string, params any) {
		write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
	}
	const native = "background-native"
	message := func(text, id string) {
		notify("session/update", map[string]any{"sessionId": native, "update": map[string]any{"sessionUpdate": "agent_message_chunk", "messageId": id, "content": map[string]string{"type": "text", "text": text}}})
	}
	done := make(chan struct{})
	go func() {
		for cycle := 1; ; cycle++ {
			wait := func(kind string) bool {
				for {
					if _, err := os.Stat(filepath.Join(dir, fmt.Sprintf("%s-%d", kind, cycle))); err == nil {
						return true
					}
					select {
					case <-done:
						return false
					case <-time.After(10 * time.Millisecond):
					}
				}
			}
			if !wait("start") {
				return
			}
			notify("_claude/sdkMessage", map[string]any{"sessionId": native, "message": map[string]string{"type": "system", "subtype": "session_state_changed", "state": "running", "uuid": fmt.Sprintf("cycle-%d", cycle)}})
			notify("session/update", map[string]any{"sessionId": native, "update": map[string]any{"sessionUpdate": "user_message_chunk", "content": map[string]string{"type": "text", "text": "<task-notification>internal provider notification</task-notification>"}}})
			message(fmt.Sprintf("BACKGROUND-%d", cycle), fmt.Sprintf("answer-%d", cycle))
			if !wait("finish") {
				return
			}
			message("-COMPLETED", fmt.Sprintf("answer-%d", cycle))
			notify("_claude/sdkMessage", map[string]any{"sessionId": native, "message": map[string]any{"type": "result", "subtype": "success", "uuid": fmt.Sprintf("result-%d", cycle), "origin": map[string]string{"kind": "task-notification"}}})
		}
	}()
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(scanner.Bytes(), &request) != nil || len(request.ID) == 0 {
			continue
		}
		var result any = map[string]any{}
		switch request.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{"sessionCapabilities": map[string]any{"resume": map[string]any{}}}, "authMethods": []any{}}
		case "session/new", "session/resume":
			result = map[string]string{"sessionId": native}
		case "session/prompt":
			message("AO-PROMPT-COMPLETED", "prompt-"+string(request.ID))
			result = map[string]string{"stopReason": "end_turn"}
		}
		write(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
	}
	close(done)
	os.Exit(0)
}

type harnessBackgroundFixture struct {
	daemon           *daemon
	session, markers string
}

func newHarnessBackgroundFixture(t *testing.T) *harnessBackgroundFixture {
	t.Helper()
	binDir := t.TempDir()
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for name, script := range map[string]string{
		"claude":     "#!/bin/sh\ncase \"$1\" in\n auth) printf '%s\\n' '{\"loggedIn\":true,\"authMethod\":\"claude.ai\",\"apiProvider\":\"firstParty\"}' ;;\n --version) printf '2.1.12 (Claude Code)\\n' ;;\n *) exit 2 ;;\nesac\n",
		"claude-acp": "#!/bin/sh\nexec \"$AO_E2E_TEST_BINARY\" -test.run='^TestFakeHarnessBackgroundProvider$'\n",
	} {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	markers := t.TempDir()
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AO_E2E_TEST_BINARY", testBinary)
	t.Setenv("AO_E2E_BACKGROUND_DIR", markers)
	t.Setenv("AO_CLAUDE_ACP_COMMAND", filepath.Join(binDir, "claude-acp"))
	t.Setenv("CLAUDE_MODEL_CONFIG", `{"availableModels":["fixture-model"]}`)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for _, name := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY"} {
		t.Setenv(name, "")
	}
	d := startDaemon(t, t.TempDir())
	project := seedProject(t, d, "harness-background")
	session := spawn(t, d, map[string]any{"projectId": project, "kind": "worker", "harness": "claude-code", "mode": "chat", "prompt": "Warmup"}).Session.ID
	d.awaitConversation(session, 30*time.Second, "initial AO prompt", func(s snapshot) bool {
		return len(s.Turns) == 1 && s.Turns[0].State == "completed" && s.Controller == "ready"
	})
	return &harnessBackgroundFixture{daemon: d, session: session, markers: markers}
}

func (f *harnessBackgroundFixture) mark(t *testing.T, kind string, cycle int) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.markers, fmt.Sprintf("%s-%d", kind, cycle)), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestHarnessBackgroundReplySurvivesReloadAndDaemonRestart(t *testing.T) {
	f := newHarnessBackgroundFixture(t)
	f.mark(t, "start", 1)
	f.daemon.awaitConversation(f.session, 10*time.Second, "live background delta", func(s snapshot) bool { return len(s.Turns) == 2 && contains(s.assistantText(), "BACKGROUND-1") })
	// Restart while the native background cycle is still streaming. The host
	// survives, replays the cycle and keeps accepting subsequent notifications.
	f.daemon.stop()
	f.daemon = startDaemon(t, f.daemon.dataDir)
	f.mark(t, "finish", 1)
	for cycle := 1; cycle <= 2; cycle++ {
		if cycle == 2 {
			f.mark(t, "start", cycle)
			f.mark(t, "finish", cycle)
		}
		s := f.daemon.awaitConversation(f.session, 15*time.Second, "completed background reply", func(s snapshot) bool { return len(s.Turns) == cycle+1 && s.Turns[cycle].State == "completed" })
		if len(s.Messages) != cycle+2 {
			t.Fatalf("duplicate or synthetic user messages: %s", describe(s))
		}
		var history struct {
			Turns []struct{ UserRequest, AgentResponse string }
			Total int
		}
		f.daemon.mustCall("GET", "/sessions/"+f.session+"/history", http.StatusOK, nil, &history)
		if history.Total != cycle+1 || len(history.Turns) != cycle+1 || history.Turns[0].AgentResponse != fmt.Sprintf("BACKGROUND-%d-COMPLETED", cycle) || history.Turns[0].UserRequest != "[Harness-triggered turn]" {
			t.Fatalf("history lost or reordered background reply: %+v", history)
		}
		command := exec.Command(buildDaemon(t), "session", "history", f.session, "--limit", "10")
		command.Env = append(os.Environ(), "AO_RUN_FILE="+filepath.Join(f.daemon.dataDir, "running.json"))
		output, err := command.CombinedOutput()
		if err != nil || strings.Count(string(output), fmt.Sprintf("BACKGROUND-%d-COMPLETED", cycle)) != 1 || strings.Contains(string(output), "<task-notification>") {
			t.Fatalf("ao session history lost/duplicated/exposed background notification: %v\n%s", err, output)
		}
		for range 2 {
			refreshed := f.daemon.conversation(f.session)
			if len(refreshed.Messages) != len(s.Messages) || !contains(refreshed.assistantText(), fmt.Sprintf("BACKGROUND-%d-COMPLETED", cycle)) {
				t.Fatalf("refresh changed durable output: %s", describe(refreshed))
			}
		}
	}
	turn, _ := send(t, f.daemon, f.session, "Followup", "ordinary-followup")
	f.daemon.awaitConversation(f.session, 10*time.Second, "ordinary post-background AO prompt", func(s snapshot) bool {
		return len(s.Turns) == 4 && s.Turns[3].State == "completed" && turnHasAnswer(s, turn, "AO-PROMPT-COMPLETED")
	})
}

func TestHarnessBackgroundBrowserFixture(t *testing.T) {
	if os.Getenv("AO_E2E_BACKGROUND_BROWSER") != "1" {
		t.Skip("set AO_E2E_BACKGROUND_BROWSER=1 for the connected renderer fixture")
	}
	f := newHarnessBackgroundFixture(t)
	done := make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		switch r.URL.Path {
		case "/test/background":
			_ = json.NewEncoder(w).Encode(map[string]string{"baseUrl": strings.TrimSuffix(f.daemon.baseURL, "/api/v1"), "sessionId": f.session})
		case "/test/background/start":
			f.mark(t, "start", 1)
			w.WriteHeader(http.StatusNoContent)
		case "/test/background/finish":
			f.mark(t, "finish", 1)
			w.WriteHeader(http.StatusNoContent)
		case "/test/shutdown":
			once.Do(func() { close(done) })
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	fmt.Printf("AO_BACKGROUND_FIXTURE=%s\n", server.URL)
	select {
	case <-done:
	case <-time.After(20 * time.Minute):
		t.Fatal("background browser fixture timed out")
	}
}
