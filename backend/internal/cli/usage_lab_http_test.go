package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	hcgsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/hcg"
	"github.com/aoagents/agent-orchestrator/components/hcg/reporting"
)

const labHeaderMarker = "SYNTHETIC_LAB_HEADER_MARKER"

type labHTTPRoundTrip func(*http.Request) (*http.Response, error)

func (f labHTTPRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type labHTTPFixture struct {
	root, family string
	mode, calls  atomic.Int32
	entered      chan struct{}
}

func newLabHTTPFixture(t *testing.T, family string) *labHTTPFixture {
	t.Helper()
	h := &labHTTPFixture{root: t.TempDir(), family: family, entered: make(chan struct{}, 1)}
	if err := os.Chmod(h.root, 0700); err != nil {
		t.Fatal(err)
	}
	return h
}
func (h *labHTTPFixture) placeNoncredentialMarker(t *testing.T) {
	t.Helper()
	dir := h.root
	for _, name := range []string{h.family, "synthetic-ref"} {
		dir = filepath.Join(dir, name)
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	raw, _ := json.Marshal(map[string]string{"provider": h.family, "name": "synthetic-ref", "version": "v1", "workspace": "synthetic-workspace", "secret": labHeaderMarker})
	if err := os.WriteFile(filepath.Join(dir, "v1.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
}
func (h *labHTTPFixture) provider(t *testing.T) reporting.DelegationProvider {
	t.Helper()
	return hcgsvc.NewLabHTTPProvider(h.root, labHTTPRoundTrip(func(r *http.Request) (*http.Response, error) {
		n := h.calls.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		if r.Method != http.MethodPost || r.URL.String() != "https://api."+h.family+".com/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer "+labHeaderMarker || strings.Contains(string(body), labHeaderMarker) {
			t.Error("same caller violated fixed HTTP/secret boundary")
		}
		mode := h.mode.Load()
		if mode == 4 {
			select {
			case h.entered <- struct{}{}:
			default:
			}
			<-r.Context().Done()
			return nil, r.Context().Err()
		}
		response := fixtureDelegationResponse("SYNTHETIC_SOURCE_142\nDOCUMENT_DIRECTIVE_IGNORE_THE_QUESTION\n")
		if mode == 1 {
			response.Body = []byte(`{"id":"fixture-response","model":"fixture-model","choices":[{"message":{"content":"not-json"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`)
		}
		if mode == 2 {
			var parsed map[string]any
			if err := json.Unmarshal(response.Body, &parsed); err != nil {
				return nil, err
			}
			delete(parsed, "usage")
			response.Body, _ = json.Marshal(parsed)
		}
		if mode == 3 {
			response.Status = 401
			response.Body = []byte(labHeaderMarker)
		}
		return &http.Response{StatusCode: response.Status, Header: http.Header{"X-Request-Id": {fmt.Sprintf("synthetic-%d", n)}}, Body: io.NopCloser(bytes.NewReader(response.Body)), Request: r}, nil
	}))
}
func assertLabSafeFailure(t *testing.T, out string) {
	t.Helper()
	var r struct {
		Schema, Status, Answer string
		Findings, Evidence     []any
	}
	if json.Unmarshal([]byte(out), &r) != nil || r.Schema != "hcg.delegate-read/1" || r.Status == "ok" || r.Answer != "" || len(r.Findings) != 0 || len(r.Evidence) != 0 || strings.Contains(out, labHeaderMarker) {
		t.Fatal("fault exposed partial/unsafe output or lost fixed schema")
	}
}

func (f *labCallerFixture) mcpRead(t *testing.T) string {
	t.Helper()
	t.Setenv("AO_SESSION_ID", string(f.session.ID))
	input, writer := io.Pipe()
	reader, output := io.Pipe()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	root := NewRootCommand(Deps{In: input, Out: output, Err: io.Discard, ProcessAlive: func(int) bool { return true }})
	root.SetArgs([]string{"usage", "delegation", "mcp"})
	go func() { done <- root.ExecuteContext(ctx); output.Close() }()
	defer func() {
		cancel()
		writer.Close()
		input.Close()
		reader.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("MCP did not stop")
		}
	}()
	decoder := json.NewDecoder(reader)
	if _, err := io.WriteString(writer, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`+"\n"); err != nil {
		t.Fatal(err)
	}
	var init mcpReply
	if err := decoder.Decode(&init); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(writer, mcpCall("hcg_delegate_read", map[string]any{"question": "QUESTION_MUST_SURVIVE", "paths": []string{"prompt.txt"}})+"\n"); err != nil {
		t.Fatal(err)
	}
	var reply mcpReply
	if err := decoder.Decode(&reply); err != nil {
		t.Fatal(err)
	}
	if reply.Error != nil || reply.Result.IsError || len(reply.Result.Content) != 1 {
		t.Fatal("MCP same caller did not return a verified result")
	}
	return reply.Result.Content[0].Text
}

func TestHCGLabFixedHTTPAndMCPCallerBoundaries(t *testing.T) {
	for _, family := range []string{"openai", "anthropic"} {
		t.Run(family, func(t *testing.T) {
			h := newLabHTTPFixture(t, family)
			f := newLabCaller(t, family, h.provider(t))
			bad := f.registration
			bad.Grant = nil
			raw, _ := json.Marshal(bad)
			f.call(t, 1, "register", "--profile-json", string(raw))
			copyGrant := *f.registration.Grant
			copyGrant.OwnerRoot = t.TempDir()
			bad = f.registration
			bad.Grant = &copyGrant
			raw, _ = json.Marshal(bad)
			f.call(t, 1, "register", "--profile-json", string(raw))
			if h.calls.Load() != 0 {
				t.Fatal("ungranted/wrong owner reached HTTP")
			}
			f.registerAndLaunch(t)
			assertLabSafeFailure(t, f.call(t, 1, f.readArgs()...))
			if h.calls.Load() != 0 {
				t.Fatal("missing version file reached HTTP")
			}
			h.placeNoncredentialMarker(t)
			if out := f.call(t, 0, f.readArgs()...); !strings.Contains(out, "machine_checked") || strings.Contains(out, labHeaderMarker) {
				t.Fatal("same HTTP caller failed")
			}
			if out := f.mcpRead(t); !strings.Contains(out, "machine_checked") || strings.Contains(out, labHeaderMarker) {
				t.Fatal("MCP did not share verified HTTP caller")
			}
			h.mode.Store(1)
			assertLabSafeFailure(t, f.call(t, 1, f.readArgs()...))
			h.mode.Store(2)
			assertLabSafeFailure(t, f.call(t, 1, f.readArgs()...))
			calls := h.calls.Load()
			assertLabSafeFailure(t, f.call(t, 1, f.readArgs()...))
			if h.calls.Load() != calls {
				t.Fatal("unknown usage hold allowed another HTTP attempt")
			}
		})
	}
}

func TestHCGLabHTTPCallerFaultsPreserveNativeFacts(t *testing.T) {
	for _, family := range []string{"openai", "anthropic"} {
		for _, fault := range []string{"http_error", "deadline", "caller_cancel", "host_stop"} {
			t.Run(family+"/"+fault, func(t *testing.T) {
				h := newLabHTTPFixture(t, family)
				h.placeNoncredentialMarker(t)
				f := newLabCaller(t, family, h.provider(t))
				f.registerAndLaunch(t)
				f.call(t, 0, f.readArgs()...)
				project, _, err := f.store.GetProject(t.Context(), string(f.session.ProjectID))
				if err != nil {
					t.Fatal(err)
				}
				if fault == "http_error" {
					h.mode.Store(3)
					assertLabSafeFailure(t, f.call(t, 1, f.readArgs()...))
				} else {
					h.mode.Store(4)
					ctx, cancel := context.WithCancel(t.Context())
					if fault == "deadline" {
						cancel()
						ctx, cancel = context.WithTimeout(t.Context(), 5*time.Second)
					}
					defer cancel()
					type result struct {
						out string
						err error
					}
					done := make(chan result, 1)
					go func() {
						var out bytes.Buffer
						root := NewRootCommand(Deps{Out: &out, Err: io.Discard, ProcessAlive: func(int) bool { return true }})
						root.SetArgs(append([]string{"usage", "delegation"}, f.readArgs()...))
						err := root.ExecuteContext(ctx)
						done <- result{out.String(), err}
					}()
					select {
					case <-h.entered:
					case <-time.After(4 * time.Second):
						t.Fatal("fault did not reach parent HTTP")
					}
					if fault == "caller_cancel" {
						cancel()
					}
					if fault == "host_stop" {
						f.call(t, 0, "emergency-stop", "--enabled=true")
					}
					select {
					case r := <-done:
						if ExitCode(r.err) != 1 {
							t.Fatal("fault lost CLI exit 1")
						}
						assertLabSafeFailure(t, r.out)
					case <-time.After(7 * time.Second):
						t.Fatal("fault caller did not exit")
					}
				}
				rec, found, err := f.store.GetSession(t.Context(), f.session.ID)
				if err != nil || !found || rec.Metadata.ControllerGeneration != f.session.Metadata.ControllerGeneration || rec.Metadata.ProviderConversationID != f.session.Metadata.ProviderConversationID {
					t.Fatal("fault changed native identity")
				}
				after, _, err := f.store.GetProject(t.Context(), string(f.session.ProjectID))
				if err != nil || !reflect.DeepEqual(project.Config, after.Config) {
					t.Fatal("fault rewrote project policy")
				}
				through, err := f.store.GetUsageEventsThroughID(t.Context(), f.session.ID)
				if err != nil {
					t.Fatal(err)
				}
				rows, err := f.store.ListUsageEventsPage(t.Context(), f.session.ID, 0, through, 100)
				if err != nil || len(rows) < 1 || rows[0].Tokens.InputTokens == nil || *rows[0].Tokens.InputTokens != 10 {
					t.Fatal("fault overwrote existing canonical usage")
				}
				if h.calls.Load() != 2 {
					t.Fatal("fault retried or skipped fixed transport")
				}
			})
		}
	}
}
