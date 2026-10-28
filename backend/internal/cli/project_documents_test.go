package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProjectDocumentsCommands(t *testing.T) {
	for _, mode := range []string{"info", "read", "write", "adopt", "download"} {
		t.Run(mode, func(t *testing.T) {
			cfg := setConfigEnv(t)
			t.Setenv("AO_SESSION_ID", "owner")
			t.Setenv("AO_BROWSER_CAPABILITY", "secret")
			var calls []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(r.URL.Path, "/internal/") {
					return
				}
				calls = append(calls, r.Method+" "+r.URL.RequestURI())
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.URL.Path == "/api/v1/projects/demo":
					fmt.Fprint(w, `{"status":"ok","project":{"id":"demo","path":"/demo"}}`)
				case strings.HasSuffix(r.URL.Path, "/lease"):
					var in projectDocumentLeaseRequest
					_ = json.NewDecoder(r.Body).Decode(&in)
					if in.Actor.SessionID != "owner" || in.Actor.Capability != "secret" {
						t.Errorf("actor=%+v", in.Actor)
					}
					fmt.Fprint(w, `{"lease":{"epoch":1,"token":"lease-secret"}}`)
				case r.Method == http.MethodPut:
					var in projectDocumentWriteRequest
					_ = json.NewDecoder(r.Body).Decode(&in)
					if in.Path != "rules/a b.md" || in.ExpectedHash != "missing" || in.Content != "hello" || in.Lease.Token != "lease-secret" {
						t.Errorf("write=%+v", in)
					}
					fmt.Fprint(w, `{"file":{"path":"rules/a b.md","content":"hello","hash":"sha"}}`)
				case strings.HasSuffix(r.URL.Path, "/adopt"):
					fmt.Fprint(w, `{"adoption":{"sourceSessionId":"old","complete":true}}`)
				case strings.Contains(r.URL.Path, "/files/"):
					w.Header().Set("Content-Type", "application/octet-stream")
					_, _ = w.Write([]byte{0, 255, 1})
				case strings.HasSuffix(r.URL.Path, "/file"):
					if r.URL.Query().Get("path") != "rules/a b.md" {
						t.Error(r.URL.String())
					}
					fmt.Fprint(w, `{"file":{"path":"rules/a b.md","content":"hello","hash":"sha"}}`)
				default:
					fmt.Fprint(w, `{"documents":{"sharedDir":"/data/projects/demo/orchestrator/shared","files":[]}}`)
				}
			}))
			defer srv.Close()
			writeRunFileFor(t, cfg, srv)
			args := []string{"project", "documents", mode, "--project", "demo"}
			if mode == "read" || mode == "download" || mode == "write" {
				args = append(args, "rules/a b.md")
			}
			if mode == "adopt" {
				args = append(args, "old")
			}
			if mode == "write" {
				p := filepath.Join(t.TempDir(), "draft")
				if e := os.WriteFile(p, []byte("hello"), 0600); e != nil {
					t.Fatal(e)
				}
				args = append(args, "--file", p, "--expected-hash", "missing")
			}
			out, errOut, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, args...)
			if err != nil {
				t.Fatalf("%v %s", err, errOut)
			}
			if strings.Contains(out, "lease-secret") || strings.Contains(out, "secret") {
				t.Fatal("leaked capability")
			}
			if mode == "download" && out != string([]byte{0, 255, 1}) {
				t.Fatalf("download=%q", out)
			}
			if mode == "read" && out != "hello" {
				t.Fatalf("read=%q", out)
			}
			if len(calls) < 2 {
				t.Fatal(calls)
			}
		})
	}
}
func TestProjectDocumentWriteRequiresExplicitPrecondition(t *testing.T) {
	_, _, err := executeCLI(t, Deps{}, "project", "documents", "write", "note.md", "--file", "draft")
	if err == nil || !strings.Contains(err.Error(), "expected-hash") {
		t.Fatalf("err=%v", err)
	}
}
func TestProjectDocumentReadKeepsDaemonError(t *testing.T) {
	cfg := setConfigEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/projects/demo" {
			_, _ = io.WriteString(w, `{"project":{"id":"demo"}}`)
			return
		}
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"code":"PROJECT_DOCUMENT_ADOPTION_PENDING","message":"pending","requestId":"request-doc"}`)
	}))
	defer srv.Close()
	writeRunFileFor(t, cfg, srv)
	_, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "project", "documents", "read", "note.md", "--project", "demo")
	if err == nil || !strings.Contains(err.Error(), "PROJECT_DOCUMENT_ADOPTION_PENDING") || !strings.Contains(err.Error(), "request-doc") {
		t.Fatalf("err=%v", err)
	}
}

func TestProjectHandoffConfigRoundTrips(t *testing.T) {
	cfg := setConfigEnv(t)
	srv, capture := projectServer(t, http.StatusOK, `{"project":{"id":"demo","path":"/demo"}}`)
	writeRunFileFor(t, cfg, srv)
	_, stderr, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "project", "set-config", "demo", "--config-json", `{"orchestratorHandoff":{"sharedDir":"living","documentPath":"handoff/current.md"}}`)
	if err != nil {
		t.Fatalf("%v %s", err, stderr)
	}
	var body setConfigRequest
	if err = json.Unmarshal(capture.body, &body); err != nil {
		t.Fatal(err)
	}
	if body.Config.OrchestratorHandoff.SharedDir != "living" || body.Config.OrchestratorHandoff.DocumentPath != "handoff/current.md" {
		t.Fatalf("config=%s", capture.body)
	}
}

func TestProjectDocumentsRejectEscapingPathBeforeDaemon(t *testing.T) {
	setConfigEnv(t)
	for _, p := range []string{"../other", "/absolute", "a/../b", "rules\\secret", ""} {
		_, _, err := executeCLI(t, Deps{}, "project", "documents", "download", p, "--project", "demo")
		var misuse usageError
		if !errors.As(err, &misuse) {
			t.Errorf("path %q: expected usage error, got %v", p, err)
		}
	}
}
