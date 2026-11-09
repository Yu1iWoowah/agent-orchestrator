package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHCGDelegationMCPFixedToolAndCancellation(t *testing.T) {
	cfg := setConfigEnv(t)
	t.Setenv("AO_SESSION_ID", "synthetic-session")
	started := make(chan struct{})
	cancelled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal/telemetry/cli-invoked" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/sessions/synthetic-session/hcg-delegation/read" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		<-r.Context().Done()
		close(cancelled)
	}))
	defer server.Close()
	writeRunFileFor(t, cfg, server)
	input, writeInput := io.Pipe()
	readOutput, output := io.Pipe()
	root := NewRootCommand(Deps{In: input, Out: output, Err: io.Discard, ProcessAlive: func(int) bool { return true }})
	root.SetArgs([]string{"usage", "delegation", "mcp"})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- root.ExecuteContext(ctx); _ = output.Close() }()
	defer input.Close()
	defer writeInput.Close()
	defer readOutput.Close()
	send := func(line string) {
		t.Helper()
		if _, err := io.WriteString(writeInput, line+"\n"); err != nil {
			t.Fatal(err)
		}
	}
	decode := json.NewDecoder(readOutput)
	receive := func() mcpReply {
		t.Helper()
		var reply mcpReply
		if err := decode.Decode(&reply); err != nil {
			t.Fatal(err)
		}
		return reply
	}
	send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if reply := receive(); reply.Result.ServerInfo.Name != "hcg" {
		t.Fatalf("borrowed server identity: %+v", reply)
	}
	send(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	reply := receive()
	if len(reply.Result.Tools) != 1 || reply.Result.Tools[0].Name != "hcg_delegate_read" || reply.Result.Tools[0].Annotations["readOnlyHint"] != false {
		t.Fatalf("tool surface=%+v", reply)
	}
	for _, key := range []string{"provider", "model", "profile", "session", "env", "executable", "root"} {
		if _, ok := reply.Result.Tools[0].InputSchema.Properties[key]; ok {
			t.Fatalf("tool accepts privileged %s", key)
		}
	}
	send(mcpCall("hcg_delegate_read", map[string]any{"question": "synthetic question", "paths": []string{"prompt.txt"}, "provider": "other"}))
	if reply := receive(); reply.Error == nil {
		t.Fatal("privileged tool argument accepted")
	}
	send(mcpCall("hcg_delegate_read", map[string]any{"question": "synthetic question", "paths": []string{"prompt.txt"}}))
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("HTTP caller not started")
	}
	send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":7}}`)
	reply = receive()
	if !reply.Result.IsError || len(reply.Result.Content) != 1 || !strings.Contains(reply.Result.Content[0].Text, `"reason":"cancelled"`) {
		t.Fatalf("cancelled reply=%+v", reply)
	}
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("MCP cancellation did not reach HTTP")
	}
	_ = writeInput.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("HCG MCP did not exit")
	}
}

func TestHCGDelegationCLIValidationAndEnvelope(t *testing.T) {
	for _, args := range [][]string{
		{"get"}, {"set", "--session", "s"}, {"set", "--session", "s", "--enabled", "TRUE"},
		{"clear"}, {"emergency-stop", "--enabled", "1"}, {"register", "--profile-json", "not-json"},
		{"read", "--session", "s", "--question", "synthetic"}, {"read", "--session", "s", "--question", "synthetic", "--path", "prompt.txt", "--output-limit", "65537"},
	} {
		_, _, err := executeCLI(t, Deps{}, append([]string{"usage", "delegation"}, args...)...)
		if ExitCode(err) != 2 {
			t.Fatalf("%v exit=%d err=%v", args, ExitCode(err), err)
		}
	}
	cfg := setConfigEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal/telemetry/cli-invoked" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.URL.Path != "/api/v1/sessions/s/hcg-delegation" {
			t.Errorf("unexpected route: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"code":"SYNTHETIC_POLICY_CONFLICT","message":"synthetic conflict","requestId":"synthetic-request"}`)
	}))
	defer srv.Close()
	writeRunFileFor(t, cfg, srv)
	_, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "usage", "delegation", "get", "--session", "s")
	if ExitCode(err) != 1 || !strings.Contains(err.Error(), "SYNTHETIC_POLICY_CONFLICT") || !strings.Contains(err.Error(), "synthetic-request") {
		t.Fatalf("lost daemon envelope: %v", err)
	}
}

type delegationFixtureTimeout struct{}

func (delegationFixtureTimeout) Error() string   { return "synthetic HTTP client timeout" }
func (delegationFixtureTimeout) Timeout() bool   { return true }
func (delegationFixtureTimeout) Temporary() bool { return false }

type delegationTimeoutTransport struct{}

func (delegationTimeoutTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Path == "/internal/telemetry/cli-invoked" {
		return &http.Response{StatusCode: http.StatusNoContent, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	}
	if r.Context().Err() != nil {
		return nil, r.Context().Err()
	}
	return nil, delegationFixtureTimeout{}
}
func TestHCGDelegationHTTPClientTimeoutReturnsSafeJSON(t *testing.T) {
	cfg := setConfigEnv(t)
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("injected transport should handle request") }))
	defer server.Close()
	writeRunFileFor(t, cfg, server)
	out, _, err := executeCLI(t, Deps{HTTPClient: &http.Client{Transport: delegationTimeoutTransport{}}, ProcessAlive: func(int) bool { return true }}, "usage", "delegation", "read", "--session", "s", "--question", "synthetic question", "--path", "prompt.txt", "--json")
	var reply struct {
		Schema, Status, Reason, Answer string
		Findings, Evidence             []any
	}
	if ExitCode(err) != 1 || json.Unmarshal([]byte(out), &reply) != nil || reply.Schema != "hcg.delegate-read/1" || reply.Status != "unavailable" || reply.Reason != "deadline_exceeded" || reply.Answer != "" || len(reply.Findings) != 0 || len(reply.Evidence) != 0 {
		t.Fatal("HTTP client timeout did not return fixed unavailable JSON/exit 1")
	}
}
