package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHCGDelegationMCPFixedToolAndCancellation(t *testing.T) {
	cfg := setConfigEnv(t)
	t.Setenv("AO_SESSION_ID", "synthetic-session")
	started := make(chan struct{})
	cancelled := make(chan struct{})
	server := newFixtureServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	root := newTestRootCommand(t, Deps{In: input, Out: output, Err: io.Discard, HTTPClient: apiFixtureClient(t, nil), ProcessAlive: func(int) bool { return true }})
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
	call := `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"hcg_delegate_read","arguments":{"question":"synthetic question","paths":["prompt.txt"]},"_meta":{"progressToken":"different-from-id"}}}`
	send(call)
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("HTTP caller not started")
	}
	send(call)
	if duplicate := receive(); duplicate.Error == nil || duplicate.Error.Code != -32600 {
		t.Fatalf("duplicate active ID accepted: %+v", duplicate)
	}
	send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":7,"reason":"synthetic cancellation","_meta":{"progressToken":"not-the-request-id"}}}`)
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
	srv := newFixtureServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	server := newFixtureServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("injected transport should handle request") }))
	defer server.Close()
	writeRunFileFor(t, cfg, server)
	out, _, err := executeCLI(t, Deps{HTTPClient: terminalFixtureClient(t, delegationTimeoutTransport{}.RoundTrip), ProcessAlive: func(int) bool { return true }}, "usage", "delegation", "read", "--session", "s", "--question", "synthetic question", "--path", "prompt.txt", "--json")
	var reply struct {
		Schema, Status, Reason, Answer string
		Findings, Evidence             []any
	}
	if ExitCode(err) != 1 || json.Unmarshal([]byte(out), &reply) != nil || reply.Schema != "hcg.delegate-read/1" || reply.Status != "unavailable" || reply.Reason != "deadline_exceeded" || reply.Answer != "" || len(reply.Findings) != 0 || len(reply.Evidence) != 0 {
		t.Fatal("HTTP client timeout did not return fixed unavailable JSON/exit 1")
	}
}

// Synthetic wire shapes from HCG-LAB-FIX1-delta §4.1: Codex rust-v0.167.0
// trace metadata and Claude Code 2.1.319 numeric onprogress token. These are
// source-derived fixtures, not captured native sessions or provider traffic.
func TestHCGDelegationMCPMetadataEnvelope(t *testing.T) {
	const args = `{"question":"Find the synthetic call chain and cite each edge.","paths":["C-a/entry.txt","C-a/middle.txt","C-a/leaf.txt"]}`
	const canary = "SYNTHETIC_METADATA_CANARY"
	tests := []struct {
		name, meta, outer, arguments, tool, client, message string
	}{
		{name: "F-Codex", client: "codex", meta: `{"traceparent":"00-11111111111111111111111111111111-2222222222222222-01","tracestate":"synthetic=1"}`},
		{name: "F-Claude", client: "claude-code", meta: `{"progressToken":12}`},
		{name: "F-Meta-string", meta: `{"progressToken":"synthetic-progress","vendor.example/note":{"x":1}}`},
		{name: "F-Meta-empty", meta: `{}`},
		{name: "F-Meta-omitted"},
		{name: "F-Outer-meta-only", meta: `{"clientHint":{"x":1},"provider":"ignored","session":"ignored","note":"` + canary + `"}`},
	}
	for _, key := range []string{"clientHint", "provider", "session"} {
		tests = append(tests, struct{ name, meta, outer, arguments, tool, client, message string }{
			name: "F-Outer-rejected-" + key, outer: `,"` + key + `":"` + canary + `"`, message: "invalid HCG tool request",
		})
	}
	for _, meta := range []struct{ name, value string }{
		{"null", `null`}, {"array", `[]`}, {"string", `"` + canary + `"`}, {"number", `1`}, {"bool", `false`},
		{"token-object", `{"progressToken":{"note":"` + canary + `"}}`}, {"token-bool", `{"progressToken":true}`},
		{"token-null", `{"progressToken":null,"note":"` + canary + `"}`}, {"token-array", `{"progressToken":[],"note":"` + canary + `"}`},
	} {
		tests = append(tests, struct{ name, meta, outer, arguments, tool, client, message string }{
			name: "N-Meta-" + meta.name, meta: meta.value, message: "invalid HCG tool request",
		})
	}
	for _, key := range []string{"provider", "model", "profile", "session", "env", "executable", "root", "_meta"} {
		tests = append(tests, struct{ name, meta, outer, arguments, tool, client, message string }{
			name: "N-Args-" + key, meta: `{}`, arguments: strings.TrimSuffix(args, "}") + `,"` + key + `":"` + canary + `"}`, message: "invalid HCG tool arguments",
		})
	}
	for _, bad := range []string{`{"paths":["prompt.txt"]}`, `{"question":"synthetic"}`, `{"question":1,"paths":["prompt.txt"]}`, `{"question":"synthetic","paths":"prompt.txt"}`} {
		tests = append(tests, struct{ name, meta, outer, arguments, tool, client, message string }{
			name: "N-Args-required-type-" + bad, meta: `{}`, arguments: bad, message: "invalid HCG tool arguments",
		})
	}
	tests = append(tests, struct{ name, meta, outer, arguments, tool, client, message string }{
		name: "N-tool", meta: `{}`, tool: "other", message: "invalid HCG tool request",
	})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := setConfigEnv(t)
			t.Setenv("AO_SESSION_ID", "synthetic-session")
			var calls atomic.Int32
			server := newFixtureServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/internal/telemetry/cli-invoked" {
					w.WriteHeader(http.StatusNoContent)
					return
				}
				calls.Add(1)
				body, err := io.ReadAll(r.Body)
				if err != nil || r.Method != http.MethodPost || r.URL.Path != "/api/v1/sessions/synthetic-session/hcg-delegation/read" {
					t.Errorf("unexpected HTTP request: %s %s %v", r.Method, r.URL.Path, err)
				}
				var want, got any
				_ = json.Unmarshal([]byte(args), &want)
				_ = json.Unmarshal(body, &got)
				wantJSON, _ := json.Marshal(want)
				gotJSON, _ := json.Marshal(got)
				if !bytes.Equal(wantJSON, gotJSON) {
					t.Errorf("metadata changed HTTP arguments: %s", gotJSON)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"schemaVersion":"hcg.delegate-read/1","status":"not_found"}`)
			}))
			defer server.Close()
			input, writer := io.Pipe()
			reader, output := io.Pipe()
			defer input.Close()
			defer writer.Close()
			defer reader.Close()
			writeRunFileFor(t, cfg, server)
			var stderr bytes.Buffer
			root := newTestRootCommand(t, Deps{In: input, Out: output, Err: &stderr, HTTPClient: apiFixtureClient(t, nil), ProcessAlive: func(int) bool { return true }})
			root.SetArgs([]string{"usage", "delegation", "mcp"})
			done := make(chan error, 1)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			go func() { <-ctx.Done(); _ = writer.Close(); _ = reader.Close() }()
			go func() { done <- root.ExecuteContext(ctx); _ = output.Close() }()
			decoder := json.NewDecoder(reader)
			var stdout strings.Builder
			exchange := func(line string) mcpReply {
				t.Helper()
				if _, err := io.WriteString(writer, line+"\n"); err != nil {
					t.Fatal(err)
				}
				var raw json.RawMessage
				if err := decoder.Decode(&raw); err != nil {
					t.Fatal(err)
				}
				stdout.Write(raw)
				var reply mcpReply
				if err := json.Unmarshal(raw, &reply); err != nil {
					t.Fatal(err)
				}
				return reply
			}
			client := tt.client
			if client == "" {
				client = "synthetic-client"
			}
			init := exchange(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"` + client + `","version":"synthetic"}}}`)
			if string(init.ID) != "1" || init.Result.ProtocolVersion != "2025-06-18" || init.Result.Capabilities["tools"] == nil || init.Result.ServerInfo.Name != "hcg" {
				t.Fatalf("handshake=%+v", init)
			}
			_, _ = io.WriteString(writer, `{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n")
			tools := exchange(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{"cursor":"synthetic","_meta":{"note":"` + canary + `"}}}`)
			if string(tools.ID) != "2" || len(tools.Result.Tools) != 1 || tools.Result.Tools[0].Name != "hcg_delegate_read" || !strings.Contains(stdout.String(), `"additionalProperties":false`) {
				t.Fatalf("tool discovery=%+v", tools)
			}
			arguments, tool := tt.arguments, tt.tool
			if arguments == "" {
				arguments = args
			}
			if tool == "" {
				tool = "hcg_delegate_read"
			}
			params := `{"name":"` + tool + `","arguments":` + arguments
			if tt.meta != "" {
				params += `,"_meta":` + tt.meta
			}
			reply := exchange(`{"jsonrpc":"2.0","id":12,"method":"tools/call","params":` + params + tt.outer + `}}`)
			_ = writer.Close()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if string(reply.ID) != "12" {
				t.Fatalf("lost request ID: %+v", reply)
			}
			if tt.message != "" {
				if reply.Error == nil || reply.Error.Code != -32602 || reply.Error.Message != tt.message || calls.Load() != 0 {
					t.Fatalf("unsafe rejection: reply=%+v calls=%d", reply, calls.Load())
				}
			} else if reply.Error != nil || reply.Result.IsError || len(reply.Result.Content) != 1 || !strings.Contains(reply.Result.Content[0].Text, `"schemaVersion":"hcg.delegate-read/1"`) || calls.Load() != 1 {
				t.Fatalf("valid envelope rejected: reply=%+v calls=%d", reply, calls.Load())
			}
			if strings.Contains(stdout.String(), canary) || stderr.Len() != 0 || strings.Contains(stdout.String(), "synthetic-progress") || strings.Contains(stdout.String(), "traceparent") {
				t.Fatalf("metadata leaked: stdout=%s stderr=%s", stdout.String(), stderr.String())
			}
		})
	}
}
