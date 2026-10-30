//go:build e2e && linux

package cli_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
)

//go:embed testdata/account-provider.py
var accountProviderFixture []byte

func TestE2E_AccountScopesHandoffRestartAndSecretSinks(t *testing.T) {
	root, err := os.MkdirTemp(os.TempDir(), "ac-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	t.Cleanup(func() { stopAccountFixtureProcesses(root) })
	for _, n := range []string{"h", "t", "bin", "state"} {
		if err = os.Mkdir(filepath.Join(root, n), 0700); err != nil {
			t.Fatal(err)
		}
	}
	home := filepath.Join(root, "h")
	device := map[string][32]byte{}
	for _, p := range []string{"codex", "claude"} {
		d := filepath.Join(home, "."+p)
		_ = os.Mkdir(d, 0700)
		f := "auth.json"
		if p == "claude" {
			f = ".credentials.json"
		}
		path := filepath.Join(d, f)
		body := []byte(`{"tokens":{"account_id":"fake-device-codex","access_token":"ACCOUNT_E2E_DEVICE_TOKEN_CANARY"}}`)
		_ = os.WriteFile(path, body, 0600)
		device[path] = sha256.Sum256(body)
		if err = os.WriteFile(filepath.Join(root, "bin", p), accountProviderFixture, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"gh", "copilot", "opencode", "cursor", "aider", "goose", "amp", "pi", "droid", "codex-acp", "claude-agent-acp"} {
		_ = os.WriteFile(filepath.Join(root, "bin", name), []byte("#!/bin/sh\nexit 127\n"), 0700)
	}
	deviceSnapshot := func() map[string]string {
		result := map[string]string{}
		for _, provider := range []string{"codex", "claude"} {
			base := filepath.Join(home, "."+provider)
			_ = filepath.WalkDir(base, func(path string, entry os.DirEntry, walkErr error) error {
				if walkErr != nil {
					t.Fatal(walkErr)
				}
				info, err := os.Lstat(path)
				if err != nil {
					t.Fatal(err)
				}
				key, _ := filepath.Rel(home, path)
				value := info.Mode().String()
				if st, ok := info.Sys().(*syscall.Stat_t); ok {
					value += fmt.Sprintf(":%d:%d:%d", st.Uid, st.Ino, st.Nlink)
				}
				if info.Mode()&os.ModeSymlink != 0 {
					target, _ := os.Readlink(path)
					value += ":" + target
				} else if !info.IsDir() {
					data, _ := os.ReadFile(path)
					value += fmt.Sprintf(":%x", sha256.Sum256(data))
				}
				result[key] = value
				return nil
			})
		}
		return result
	}
	deviceBefore := deviceSnapshot()
	e := env{runFile: filepath.Join(root, "r.json"), dataDir: filepath.Join(root, "state"), port: freePort(t)}
	environ := e.environ("")
	environ = append(environ, "HOME="+home, "TMPDIR="+filepath.Join(root, "t"), "PATH="+filepath.Join(root, "bin")+":"+os.Getenv("PATH"), "ACCOUNT_E2E_ROOT="+root, "OPENAI_API_KEY=ACCOUNT_E2E_AMBIENT_TOKEN_CANARY", "ANTHROPIC_API_KEY=ACCOUNT_E2E_AMBIENT_TOKEN_CANARY")
	runCode := func(args ...string) (string, int) {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, aoBin, args...)
		cmd.Env = environ
		data, err := cmd.CombinedOutput()
		if ctx.Err() != nil {
			t.Fatalf("command timed out: %v", args)
		}
		code := 0
		if err != nil {
			var ee *exec.ExitError
			if !asExit(err, &ee) {
				t.Fatal(err)
			}
			code = ee.ExitCode()
		}
		if bytes.Contains(data, []byte("TOKEN_CANARY")) {
			t.Fatal("CLI secret sink")
		}
		t.Logf("$ ao %s\n%s(exit %d)", strings.Join(args, " "), data, code)
		return string(data), code
	}
	log, err := os.OpenFile(filepath.Join(root, "daemon.log"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	readLog := func() string { data, _ := os.ReadFile(log.Name()); return string(data) }
	var daemon *exec.Cmd
	var done chan error
	start := func() {
		daemon = exec.Command(aoBin, "daemon")
		daemon.Env = environ
		daemon.Stdout = log
		daemon.Stderr = log
		if er := daemon.Start(); er != nil {
			t.Fatal(er)
		}
		done = make(chan error, 1)
		go func() { done <- daemon.Wait() }()
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			r, er := http.Get(fmt.Sprintf("http://127.0.0.1:%d/readyz", e.port))
			if er == nil {
				_ = r.Body.Close()
				if r.StatusCode == 200 {
					return
				}
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("private daemon not ready: %s", readLog())
	}
	run := func(args ...string) string {
		out, code := runCode(args...)
		if code != 0 {
			t.Fatalf("command failed %v: %s", args, out)
		}
		return out
	}
	stop := func() {
		if daemon == nil {
			return
		}
		out, code := runCode("stop")
		if code != 0 {
			t.Errorf("stop: %s", out)
		}
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = daemon.Process.Kill()
			<-done
			t.Error("daemon failed shutdown")
		}
		daemon = nil
	}
	t.Cleanup(stop)
	t.Cleanup(func() {
		if t.Failed() {
			text := strings.ReplaceAll(readLog(), "TOKEN_CANARY", "REDACTED")
			if len(text) > 10000 {
				text = text[len(text)-10000:]
			}
			t.Log("private daemon diagnostics: " + text)
		}
	})
	start()
	api := func(method, path string, body any, expectedStatus ...int) map[string]any {
		var input io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			input = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, fmt.Sprintf("http://127.0.0.1:%d/api/v1%s", e.port, path), input)
		req.Header.Set("Content-Type", "application/json")
		r, er := http.DefaultClient.Do(req)
		if er != nil {
			t.Fatal(er)
		}
		defer r.Body.Close()
		data, _ := io.ReadAll(r.Body)
		if (len(expectedStatus) > 0 && r.StatusCode != expectedStatus[0]) || (len(expectedStatus) == 0 && r.StatusCode >= 300) {
			t.Fatalf("API %s %s: %d %s", method, path, r.StatusCode, data)
		}
		if bytes.Contains(data, []byte("TOKEN_CANARY")) {
			t.Fatalf("API secret sink at %s", path)
		}
		var result map[string]any
		if len(data) > 0 {
			if er = json.Unmarshal(data, &result); er != nil {
				t.Fatal(er)
			}
		}
		return result
	}
	add := func(provider, email string) string {
		_ = os.WriteFile(filepath.Join(root, "next-identity"), []byte(email), 0600)
		_ = os.WriteFile(filepath.Join(root, "approve-login"), nil, 0600)
		out := run("account", "add", "--provider", provider, "--no-attach", "--json")
		at := strings.Index(out, "{")
		var op struct {
			ResultAccountID string `json:"resultAccountId"`
			Phase           string `json:"phase"`
			Outcome         string `json:"outcome"`
		}
		if at < 0 || json.Unmarshal([]byte(out[at:]), &op) != nil || op.Phase != "complete" || op.Outcome != "complete" || op.ResultAccountID == "" {
			t.Fatalf("not confirmed: %s", out)
		}
		return op.ResultAccountID
	}
	// This route waits for local-store initialization; /readyz only publishes
	// the listener. Do not race catalog directory creation/protection below.
	api("GET", "/agents/codex/accounts", nil)
	// Reproduce the audit81 unresolved boundary with explicit metadata evidence.
	legacyID := "b60a377d-da68-4a61-86f2-f31f04c571f2"
	legacyParent := filepath.Join(e.dataDir, "harnesses", "codex")
	legacyRoot := filepath.Join(legacyParent, "accounts")
	legacyHome := filepath.Join(legacyRoot, legacyID, "credential-home")
	if err := os.MkdirAll(legacyHome, 0700); err != nil {
		t.Fatal(err)
	}
	desc, _ := json.Marshal(map[string]any{"version": 2, "id": legacyID, "source": "managed", "authMethod": "chatgpt", "accountEmail": "legacy@example.test", "createdAt": time.Now().UTC(), "verifiedAt": time.Now().UTC()})
	if err := os.WriteFile(filepath.Join(legacyRoot, legacyID, "account.json"), desc, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyHome, "auth.json"), []byte(`{"tokens":{"account_id":"fake-legacy","access_token":"ACCOUNT_E2E_PRIVATE_TOKEN_CANARY"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(root, "next-identity"), []byte("legacy@example.test"), 0600)
	_ = os.WriteFile(filepath.Join(root, "approve-login"), nil, 0600)
	if err := os.Chmod(legacyParent, 0775); err != nil {
		t.Fatal(err)
	}
	var detached struct {
		OperationID string `json:"operationId"`
	}
	if err := json.Unmarshal([]byte(run("account", "add", "--provider", "codex", "--detach", "--json")), &detached); err != nil || detached.OperationID == "" {
		t.Fatal("missing detached reconnect reference", err)
	}
	finalizedDeadline := time.Now().Add(10 * time.Second)
	// Force the same read-only probe that startup can run concurrently with
	// a managed login. It must not change the device identity into a duplicate.
	probeCtx, probeCancel := context.WithDeadline(context.Background(), finalizedDeadline)
	probe := exec.CommandContext(probeCtx, filepath.Join(root, "bin", "codex"), "login", "status")
	probe.Env = environ
	probeOutput, probeErr := probe.CombinedOutput()
	probeCancel()
	if probeErr != nil || !strings.Contains(string(probeOutput), "Logged in using ChatGPT") || !reflect.DeepEqual(deviceBefore, deviceSnapshot()) {
		t.Fatal("read-only background probe mutated the device or failed")
	}
	// A status probe or another login cannot satisfy this operation's marker.
	finalized := filepath.Join(root, "login-finalized", "codex", detached.OperationID)
	for {
		if _, err := os.Stat(finalized); err == nil {
			break
		}
		if !time.Now().Before(finalizedDeadline) {
			t.Fatal("this login did not reach its finalization checkpoint")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Make an ancestor unsafe: catalog refresh may protect its own root, but
	// must reject a writable ancestor rather than repair it. Check the exact
	// precondition before and after the RPC, with no retry/alternate outcome.
	assertUnsafeParent := func() {
		info, err := os.Stat(legacyParent)
		if err != nil || info.Mode().Perm() != 0775 {
			t.Fatal("unsafe legacy ancestor precondition changed")
		}
	}
	assertUnsafeParent()
	failure := api("POST", "/agents/accounts/login/"+detached.OperationID+"/verify", struct{}{}, http.StatusServiceUnavailable)
	if failure["code"] != "AGENT_ACCOUNT_STORAGE_UNAVAILABLE" {
		t.Fatalf("unsafe legacy root lost canonical error: %v", failure)
	}
	assertUnsafeParent()
	if err := os.Chmod(legacyParent, 0700); err != nil {
		t.Fatal(err)
	}
	legacySnapshot := api("GET", "/agents/codex/accounts", nil)
	snapshotJSON, _ := json.Marshal(legacySnapshot)
	t.Logf("safe legacy metadata: %s", snapshotJSON)
	conflict, code := runCode("account", "login", "--operation", detached.OperationID, "--no-attach")
	if code != 1 || !strings.Contains(conflict, "AGENT_ACCOUNT_LEGACY_IDENTITY_CONFLICT") || strings.Contains(conflict, "operation retained") || !strings.Contains(conflict, "Settings") {
		t.Fatalf("safe legacy root expected canonical conflict: %d %s", code, conflict)
	}
	api("POST", "/agents/codex/accounts/"+legacyID+"/logout", nil)
	api("DELETE", "/agents/codex/accounts/"+legacyID, nil)
	recovered := add("codex", "legacy@example.test")
	run("account", "rm", recovered)
	// Real CLI terminal + Ctrl+C: no fake native token reaches the terminal,
	// and cancellation must complete before the process reports cleanup.
	_ = os.Remove(filepath.Join(root, "approve-login"))
	_ = os.WriteFile(filepath.Join(root, "hold-login"), nil, 0600)
	_ = os.WriteFile(filepath.Join(root, "next-identity"), []byte("held@example.test"), 0600)
	interactive := exec.Command(aoBin, "account", "add", "--provider", "codex")
	interactive.Env = environ
	terminal, err := pty.Start(interactive)
	if err != nil {
		t.Fatal(err)
	}
	chunks := make(chan string, 32)
	go func() {
		defer close(chunks)
		buffer := make([]byte, 4096)
		for {
			n, er := terminal.Read(buffer)
			if n > 0 {
				chunks <- string(buffer[:n])
			}
			if er != nil {
				return
			}
		}
	}()
	transcriptOutput := ""
	deadlinePrompt := time.NewTimer(20 * time.Second)
waiting:
	for {
		select {
		case chunk, ok := <-chunks:
			if !ok {
				t.Fatal("interactive login exited before prompt")
			}
			transcriptOutput += chunk
			if strings.Contains(transcriptOutput, "Paste authorization code: ") {
				break waiting
			}
		case <-deadlinePrompt.C:
			_ = interactive.Process.Kill()
			_ = terminal.Close()
			_ = interactive.Wait()
			t.Fatal("login prompt unavailable")
		}
	}
	_ = deadlinePrompt.Stop()
	if strings.Contains(transcriptOutput, "TOKEN_CANARY") || strings.Contains(transcriptOutput, "~") {
		t.Fatal("native login token printed")
	}
	_, _ = terminal.Write([]byte("ABCD-1234"))
	echoDeadline := time.NewTimer(10 * time.Second)
	for !strings.Contains(transcriptOutput, "ABCD-1234") {
		select {
		case chunk, ok := <-chunks:
			if !ok {
				t.Fatal("login exited before input echo")
			}
			transcriptOutput += chunk
		case <-echoDeadline.C:
			t.Fatal("input echo without newline was delayed")
		}
	}
	echoDeadline.Stop()
	_ = os.WriteFile(filepath.Join(root, "approve-login"), nil, 0600)
	credentialDeadline := time.Now().Add(10 * time.Second)
	var nativePID int
	for time.Now().Before(credentialDeadline) {
		if data, err := os.ReadFile(filepath.Join(root, "credential-written")); err == nil {
			nativePID, _ = strconv.Atoi(string(data))
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if nativePID == 0 {
		t.Fatal("native provider did not reach credential-write checkpoint")
	}
	// The old 500ms verify loop would accept and terminate this writer.
	time.Sleep(750 * time.Millisecond)
	pendingID := regexp.MustCompile(`Login operation ([a-f0-9-]+)`).FindStringSubmatch(transcriptOutput)
	if len(pendingID) != 2 || api("GET", "/agents/accounts/operations/"+pendingID[1], nil)["phase"] != "pending" || syscall.Kill(nativePID, 0) != nil {
		t.Fatal("credentials accepted or native terminal closed before native exit")
	}
	_, _ = terminal.Write([]byte{3})
	interactiveDone := make(chan error, 1)
	go func() { interactiveDone <- interactive.Wait() }()
	select {
	case er := <-interactiveDone:
		if er == nil {
			t.Error("cancel should exit 1")
		}
	case <-time.After(30 * time.Second):
		_ = interactive.Process.Kill()
		<-interactiveDone
		t.Fatal("interactive cancel did not complete")
	}
	_ = terminal.Close()
	for chunk := range chunks {
		transcriptOutput += chunk
	}
	if (strings.Contains(transcriptOutput, "TOKEN_CANARY") || strings.Contains(transcriptOutput, "~")) || !strings.Contains(transcriptOutput, "cleanup confirmed") {
		t.Fatalf("unsafe or unconfirmed terminal cancel")
	}
	matchCancel := regexp.MustCompile(`Login operation ([a-f0-9-]+)`).FindStringSubmatch(transcriptOutput)
	if len(matchCancel) != 2 {
		t.Fatal("cancel operation unavailable")
	}
	cancelState := api("GET", "/agents/accounts/operations/"+matchCancel[1], nil)
	if cancelState["phase"] != "complete" || cancelState["outcome"] != "cancelled" {
		t.Fatalf("cancel not durable: %v", cancelState)
	}
	cancelAccountID, _ := cancelState["accountId"].(string)
	if cancelAccountID == "" {
		t.Fatal("cancel operation lost profile identity")
	}
	if _, err := os.Lstat(filepath.Join(e.dataDir, "agent-accounts", "codex", cancelAccountID)); !os.IsNotExist(err) {
		t.Fatal("cancelled login left a managed profile")
	}
	_ = os.Remove(filepath.Join(root, "hold-login"))
	_ = os.Remove(filepath.Join(root, "credential-written"))
	// An exited, unsigned native login must release its operation without a
	// daemon restart. Follow the CLI's fresh-attempt instructions below.
	decline := filepath.Join(root, "decline-login")
	assertUnsignedCancelled := func(output string, existingID string) {
		if !strings.Contains(output, "not signed in") || !strings.Contains(output, "cleanup confirmed") {
			t.Fatal("unsigned exit did not confirm cancellation")
		}
		match := regexp.MustCompile(`Login operation ([a-f0-9-]+)`).FindStringSubmatch(output)
		if len(match) != 2 {
			t.Fatal("unsigned operation reference missing")
		}
		op := api("GET", "/agents/accounts/operations/"+match[1], nil)
		if op["phase"] != "complete" || op["outcome"] != "cancelled" {
			t.Fatalf("unsigned operation remained busy: %v", op)
		}
		accountID, _ := op["accountId"].(string)
		if accountID == "" {
			t.Fatal("cancelled operation lost account identity")
		}
		if existingID == "" {
			if !strings.Contains(output, "ao account add --provider codex") {
				t.Fatal("new-login retry instruction missing")
			}
			if _, err := os.Lstat(filepath.Join(e.dataDir, "agent-accounts", "codex", accountID)); !os.IsNotExist(err) {
				t.Fatal("unsigned new login left a pending profile")
			}
		} else if accountID != existingID || !strings.Contains(output, "ao account login "+existingID) {
			t.Fatal("existing-login retry instruction lost account identity")
		}
	}
	if err := os.WriteFile(decline, nil, 0600); err != nil {
		t.Fatal(err)
	}
	unsigned, unsignedCode := runCode("account", "add", "--provider", "codex", "--no-attach")
	if unsignedCode != 1 {
		t.Fatal("unsigned new login should exit 1")
	}
	assertUnsignedCancelled(unsigned, "")
	if err := os.Remove(decline); err != nil {
		t.Fatal(err)
	}
	retryAccount := add("codex", "retry@example.test")
	run("account", "logout", retryAccount)
	if err := os.WriteFile(decline, nil, 0600); err != nil {
		t.Fatal(err)
	}
	unsigned, unsignedCode = runCode("account", "login", retryAccount, "--no-attach")
	if unsignedCode != 1 {
		t.Fatal("unsigned re-login should exit 1")
	}
	assertUnsignedCancelled(unsigned, retryAccount)
	if err := os.Remove(decline); err != nil {
		t.Fatal(err)
	}
	retryOutput := run("account", "login", retryAccount, "--no-attach", "--json")
	var retryOperation struct {
		ResultAccountID string `json:"resultAccountId"`
		Outcome         string `json:"outcome"`
	}
	retryJSON := strings.Index(retryOutput, "{")
	if retryJSON < 0 {
		t.Fatal("fresh re-login returned no operation JSON")
	}
	if err := json.Unmarshal([]byte(retryOutput[retryJSON:]), &retryOperation); err != nil || retryOperation.Outcome != "complete" || retryOperation.ResultAccountID != retryAccount {
		t.Fatal("fresh re-login failed or changed account identity", err)
	}
	run("account", "rm", retryAccount)
	a := add("codex", "A@example.test")
	d := add("codex", "D@example.test")
	b := add("claude", "B@example.test")
	c := add("claude", "C@example.test")
	run("account", "ls", "--refresh", "--json")
	run("account", "use", a, "--default")
	run("account", "use", b, "--default")
	repo := filepath.Join(root, "repo")
	_ = os.Mkdir(repo, 0700)
	for _, args := range [][]string{{"init", "-b", "main"}, {"-c", "user.name=Fake", "-c", "user.email=fake@example.test", "commit", "--allow-empty", "-m", "Fake fixture"}} {
		cmd := exec.Command("git", args...)
		cmd.Env = environ
		cmd.Dir = repo
		if out, er := cmd.CombinedOutput(); er != nil {
			t.Fatalf("git: %v %s", er, out)
		}
	}
	api("POST", "/projects", map[string]any{"path": repo, "projectId": "accounts", "name": "Fake account scopes", "config": map[string]any{"defaultBranch": "main", "worker": map[string]string{"agent": "codex"}, "orchestrator": map[string]string{"agent": "claude-code"}, "reviewers": []map[string]string{{"harness": "claude-code"}, {"harness": "codex"}}}})
	run("account", "use", a, "--project", "accounts")
	run("account", "use", a, "--project", "accounts", "--role", "worker")
	run("account", "use", b, "--project", "accounts", "--role", "orchestrator")
	run("account", "use", c, "--project", "accounts", "--role", "reviewer", "--reviewer", "claude-code")
	cfg := api("GET", "/projects/accounts", nil)["project"].(map[string]any)["config"].(map[string]any)
	if cfg["worker"].(map[string]any)["agentAccountId"] != a || cfg["orchestrator"].(map[string]any)["agentAccountId"] != b || cfg["reviewers"].([]any)[0].(map[string]any)["agentAccountId"] != c {
		t.Fatalf("A/B/C role mismatch: %v", cfg)
	}
	mismatchOut, mismatchCode := runCode("spawn", "--standalone", "--agent", "codex", "--account", b, "--name", "Fake provider mismatch")
	if mismatchCode != 1 || !strings.Contains(mismatchOut, "AGENT_ACCOUNT_PROVIDER_MISMATCH") || !strings.Contains(mismatchOut, "[request ") {
		t.Fatalf("provider mismatch lost exit/code/request ID: %d %s", mismatchCode, mismatchOut)
	}
	run("spawn", "--project", "accounts", "--agent", "codex", "--mode", "chat", "--name", "Fake account worker", "--prompt", "Fake preserved history")
	sessions := api("GET", "/sessions", nil)["sessions"].([]any)
	if len(sessions) != 1 {
		t.Fatalf("sessions=%v", sessions)
	}
	id := sessions[0].(map[string]any)["id"].(string)
	binding := func() string { return api("GET", "/agents/accounts/sessions/"+id, nil)["codex"].(string) }
	if binding() != a {
		t.Fatal("spawn ignored role account")
	}
	run("spawn", "--standalone", "--agent", "codex", "--mode", "chat", "--account", d, "--name", "Fake explicit override", "--prompt", "Fake override")
	var overrideID string
	for _, item := range api("GET", "/sessions", nil)["sessions"].([]any) {
		session := item.(map[string]any)
		if session["displayName"] == "Fake explicit override" {
			overrideID = session["id"].(string)
		}
	}
	if overrideID == "" {
		t.Fatal("explicit override session missing")
	}
	overrideBinding := api("GET", "/agents/accounts/sessions/"+overrideID, nil)
	if overrideBinding["codex"] != d || overrideBinding["bindings"].(map[string]any)["codex"].(map[string]any)["source"] != "explicit" {
		t.Fatalf("explicit override drifted: %v", overrideBinding)
	}
	run("session", "kill", overrideID)
	run("account", "use", d, "--default")
	if binding() != a {
		t.Fatal("default changed existing binding")
	}
	conversations := func(action, identity string) []string {
		data, _ := os.ReadFile(filepath.Join(root, "invocations.jsonl"))
		var ids []string
		for _, line := range bytes.Split(data, []byte{'\n'}) {
			var call struct {
				Action       string `json:"action"`
				Identity     string `json:"identity"`
				Conversation string `json:"conversationId"`
			}
			if json.Unmarshal(line, &call) == nil && call.Action == action && call.Identity == identity {
				ids = append(ids, call.Conversation)
			}
		}
		return ids
	}
	deadline := time.Now().Add(20 * time.Second)
	for len(conversations("thread/start", "A@example.test")) == 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	startedConversations := conversations("thread/start", "A@example.test")
	if len(startedConversations) != 1 {
		t.Fatalf("worker identity/conversation not confirmed: %v", startedConversations)
	}
	nativeID := startedConversations[0]
	run("spawn", "--project", "accounts", "--kind", "orchestrator", "--mode", "tui", "--name", "Fake account orchestrator", "--prompt", "Fake orchestrator isolation")
	var orchestratorID string
	for _, item := range api("GET", "/sessions", nil)["sessions"].([]any) {
		session := item.(map[string]any)
		if session["kind"] == "orchestrator" {
			orchestratorID = session["id"].(string)
		}
	}
	if orchestratorID == "" || api("GET", "/agents/accounts/sessions/"+orchestratorID, nil)["claude"] != b {
		t.Fatal("orchestrator did not bind B")
	}
	deadline = time.Now().Add(20 * time.Second)
	for len(conversations("terminal", "B@example.test")) == 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if len(conversations("terminal", "B@example.test")) == 0 {
		t.Fatal("orchestrator did not execute as B")
	}
	run("session", "kill", orchestratorID)

	run("account", "use", d, "--session", id, "--wait")
	if binding() != d {
		t.Fatal("handoff did not commit D")
	}
	moved := conversations("thread/resume", "D@example.test")
	if len(moved) != 1 || moved[0] != nativeID {
		t.Fatalf("handoff changed native identity/history: %v", moved)
	}
	run("session", "exit-agent", id)
	stop()
	start()
	deadline = time.Now().Add(20 * time.Second)
	// /readyz only publishes the listener. Wait for startup recovery to
	// release this session's operation fence before the explicit resume.
	for {
		session := api("GET", "/sessions/"+id, nil)["session"].(map[string]any)
		if session["statusReadiness"] == "ready" {
			break
		}
		if session["statusReadiness"] != "checking" || !time.Now().Before(deadline) {
			t.Fatalf("startup recovery did not settle: %v", session["statusReadiness"])
		}
		time.Sleep(50 * time.Millisecond)
	}
	run("session", "resume-agent", id)
	for time.Now().Before(deadline) {
		snapshot := api("GET", "/sessions/"+id+"/conversation", nil)
		if len(conversations("thread/resume", "D@example.test")) >= 2 && snapshot["controller"] == "ready" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if api("GET", "/sessions/"+id+"/conversation", nil)["controller"] != "ready" {
		t.Fatal("resumed controller did not become ready")
	}
	resumed := conversations("thread/resume", "D@example.test")
	if len(resumed) != 2 || resumed[1] != nativeID {
		t.Fatalf("restart did not restore exact account/conversation: %v", resumed)
	}
	if binding() != d {
		t.Fatal("restart changed account binding")
	}
	run("account", "logout", a)
	run("account", "rm", a)
	out, code := runCode("account", "rm", d)
	if code != 1 || !strings.Contains(out, "AGENT_ACCOUNT_IN_USE") {
		t.Fatalf("active remove=%d %s", code, out)
	}
	run("session", "kill", id)
	run("account", "rm", d)
	run("account", "logout", b)
	run("account", "rm", b)
	run("account", "rm", c)
	run("account", "ls", "--json")
	streamCtx, streamCancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
	streamReq, _ := http.NewRequestWithContext(streamCtx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/api/v1/events?after=0", e.port), nil)
	streamResp, streamErr := http.DefaultClient.Do(streamReq)
	if streamErr != nil {
		streamCancel()
		t.Fatal(streamErr)
	}
	eventBytes, _ := io.ReadAll(streamResp.Body)
	_ = streamResp.Body.Close()
	streamCancel()
	if streamResp.StatusCode != 200 || !bytes.Contains(eventBytes, []byte("agent_accounts")) || bytes.Contains(eventBytes, []byte("TOKEN_CANARY")) {
		t.Fatal("SSE account events missing or secret sink")
	}
	stop()
	if !reflect.DeepEqual(deviceBefore, deviceSnapshot()) {
		t.Fatal("external fake device tree/owner/mode/inode/content changed")
	}
	for path, want := range device {
		body, er := os.ReadFile(path)
		if er != nil || sha256.Sum256(body) != want {
			t.Fatalf("external fake device changed: %s", filepath.Base(path))
		}
	}
	for _, marker := range []string{"ACCOUNT_E2E_PRIVATE_TOKEN_CANARY", "ACCOUNT_E2E_DEVICE_TOKEN_CANARY", "ACCOUNT_E2E_AMBIENT_TOKEN_CANARY"} {
		if strings.Contains(readLog(), marker) {
			t.Fatal("daemon log secret sink")
		}
		_ = filepath.WalkDir(e.dataDir, func(path string, entry os.DirEntry, er error) error {
			if er == nil && !entry.IsDir() && (strings.HasSuffix(path, ".db") || strings.HasSuffix(path, ".db-wal")) {
				body, _ := os.ReadFile(path)
				if bytes.Contains(body, []byte(marker)) {
					t.Error("DB secret sink")
				}
			}
			return nil
		})
	}
	history := filepath.Join(e.dataDir, "agent-accounts", "history", "codex", "sessions")
	entries, err := os.ReadDir(history)
	if err != nil || len(entries) == 0 {
		t.Fatalf("shared history absent: %v", err)
	}
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(history, entry.Name()))
		if err != nil || !bytes.Contains(data, []byte("Fake preserved history")) {
			t.Fatal("native history lost")
		}
	}

}

// PTY hosts intentionally survive daemon restart. Reap only this fixture's
// children after graceful stop, including a failed assertion before cancel.
func stopAccountFixtureProcesses(root string) {
	owned := func() []int {
		entries, _ := os.ReadDir("/proc")
		var pids []int
		for _, entry := range entries {
			pid, err := strconv.Atoi(entry.Name())
			if err != nil || pid == os.Getpid() {
				continue
			}
			proc := filepath.Join("/proc", entry.Name())
			info, err := os.Stat(proc)
			if err != nil {
				continue
			}
			st, ok := info.Sys().(*syscall.Stat_t)
			if !ok || int64(st.Uid) != int64(os.Getuid()) {
				continue
			}
			data, err := os.ReadFile(filepath.Join(proc, "environ"))
			if err != nil {
				continue
			}
			for _, value := range bytes.Split(data, []byte{0}) {
				if string(value) == "ACCOUNT_E2E_ROOT="+root {
					pids = append(pids, pid)
					break
				}
			}
		}
		return pids
	}
	for _, pid := range owned() {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	for range 20 {
		if len(owned()) == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, pid := range owned() {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

// Login status is an observation, even when a different managed login has
// already been approved. It must never run the credential-writing login path.
func TestE2E_AccountProviderLoginStatusDoesNotMutateDeviceOrFinalizeLogin(t *testing.T) {
	for _, signedIn := range []bool{false, true} {
		t.Run(fmt.Sprintf("signed-in=%t", signedIn), func(t *testing.T) {
			root := t.TempDir()
			home := filepath.Join(root, "h")
			device := filepath.Join(home, ".codex")
			if err := os.MkdirAll(device, 0700); err != nil {
				t.Fatal(err)
			}
			bin := filepath.Join(root, "codex")
			if err := os.WriteFile(bin, accountProviderFixture, 0700); err != nil {
				t.Fatal(err)
			}
			credential := filepath.Join(device, "auth.json")
			before := []byte(`{"fixtureEmail":"device@example.test","fakeCredential":"DEVICE_FIXTURE"}`)
			var beforeInfo os.FileInfo
			if signedIn {
				if err := os.WriteFile(credential, before, 0600); err != nil {
					t.Fatal(err)
				}
				beforeInfo, _ = os.Stat(credential)
			}
			if err := os.WriteFile(filepath.Join(root, "approve-login"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "next-identity"), []byte("different@example.test"), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, bin, "login", "status")
			cmd.Env = []string{"HOME=" + home, "ACCOUNT_E2E_ROOT=" + root, "PATH=/usr/bin:/bin"}
			out, runErr := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatal("read-only status blocked", ctx.Err())
			}
			data, readErr := os.ReadFile(credential)
			if signedIn {
				afterInfo, err := os.Stat(credential)
				if err != nil || readErr != nil || !bytes.Equal(data, before) || !os.SameFile(beforeInfo, afterInfo) || afterInfo.Mode() != beforeInfo.Mode() {
					t.Fatal("read-only status changed device credentials")
				}
			} else if !os.IsNotExist(readErr) {
				t.Fatal("read-only status created device credentials")
			}
			for _, path := range []string{filepath.Join(device, ".fixture-identity"), filepath.Join(root, "login-finalized"), filepath.Join(root, "credential-written")} {
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Fatal("read-only status created login state", filepath.Base(path))
				}
			}
			if signedIn && (runErr != nil || !strings.Contains(string(out), "Logged in using ChatGPT")) {
				t.Fatalf("signed-in observation failed: %v %s", runErr, out)
			}
			if !signedIn {
				var exit *exec.ExitError
				if !asExit(runErr, &exit) || exit.ExitCode() != 1 || !strings.Contains(string(out), "Not logged in") {
					t.Fatalf("signed-out observation failed: %v %s", runErr, out)
				}
			}
		})
	}
}
