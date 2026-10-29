package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"
)

const testAppGrant = "a6b36f36-8c38-4b5d-8505-415f15deffbc"

func writeAppCredential(t *testing.T, dir, id string) {
	t.Helper()
	name := filepath.Join(dir, "diagnostics", "grants")
	if err := os.MkdirAll(name, 0700); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]any{"grantId": id, "sessionId": "next.js-1", "projectId": "next.js", "instanceId": "app-1", "token": "test-only-secret-token-0123456789abcd", "expiresAt": time.Now().Add(time.Minute)})
	if err := os.WriteFile(filepath.Join(name, id+".json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	protectAppTestCredential(t, filepath.Join(name, id+".json"))
}
func TestAppCLIUsage(t *testing.T) {
	setConfigEnv(t)
	t.Setenv("AO_SESSION_ID", "")
	for _, args := range [][]string{{"app", "status"}, {"app", "snapshot", "get"}, {"app", "snapshot", "--host", "remote"}, {"app", "snapshot", "--window", "../bad"}, {"app", "diagnostics", "revoke"}} {
		_, _, err := executeCLI(t, Deps{}, args...)
		if ExitCode(err) != 2 {
			t.Fatalf("%v: %v", args, err)
		}
	}
}
func TestAppCLIUsesCapabilityAndPreservesPartialEvidence(t *testing.T) {
	cfg := setConfigEnv(t)
	t.Setenv("AO_SESSION_ID", "next.js-1")
	writeAppCredential(t, cfg.dataDir, testAppGrant)
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/v1/desktop/diagnostics/") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		paths = append(paths, r.Method+" "+r.URL.Path)
		if r.Header.Get("X-AO-App-Diagnostics-Capability") != "test-only-secret-token-0123456789abcd" || r.Header.Get(browserCapabilityHeader) != "" {
			t.Error("wrong capability boundary")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/desktop/diagnostics/status", "/api/v1/desktop/diagnostics/windows":
			io.WriteString(w, `{"ready":true,"windows":[{"id":"window-1","generation":1}]}`)
		case "/api/v1/desktop/diagnostics/snapshots":
			if r.Method == http.MethodPost {
				var body map[string]any
				json.NewDecoder(r.Body).Decode(&body)
				if body["projectId"] != "next.js" {
					t.Error("missing project filter")
				}
				w.WriteHeader(http.StatusCreated)
				io.WriteString(w, `{"id":"snapshot-1","snapshot":{"completeness":"partial"}}`)
			} else {
				io.WriteString(w, `{"records":[],"partial":false}`)
			}
		case "/api/v1/desktop/diagnostics/audit":
			io.WriteString(w, `{"records":[],"partial":false}`)
		default:
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `{"code":"APP_DIAGNOSTICS_CAPABILITY_INVALID","message":"Denied","requestId":"request-42"}`)
		}
	}))
	defer srv.Close()
	writeRunFileFor(t, cfg, srv)
	deps := Deps{ProcessAlive: func(int) bool { return true }}
	for _, args := range [][]string{{"app", "status", "--json"}, {"app", "windows", "--json"}, {"app", "snapshot", "list", "--json"}, {"app", "diagnostics", "audit", "--json"}} {
		out, _, err := executeCLI(t, deps, args...)
		if err != nil || strings.Contains(out, "secret-token") {
			t.Fatalf("%v out=%s err=%v", args, out, err)
		}
	}
	out, _, err := executeCLI(t, deps, "app", "snapshot", "--project", "next.js", "--require-complete", "--json")
	if ExitCode(err) != 1 || !strings.Contains(out, `"completeness": "partial"`) {
		t.Fatalf("partial evidence lost: %s %v", out, err)
	}
	_, _, err = executeCLI(t, deps, "app", "snapshot", "get", "snapshot-1", "--json")
	var api apiResponseError
	if !errors.As(err, &api) || api.ErrorBody.RequestID != "request-42" {
		t.Fatalf("envelope lost: %v", err)
	}
	if len(paths) != 6 {
		t.Fatalf("unexpected retries: %v", paths)
	}
}
func TestAppCredentialAmbiguityAndPathSafety(t *testing.T) {
	cfg := setConfigEnv(t)
	t.Setenv("AO_SESSION_ID", "next.js-1")
	writeAppCredential(t, cfg.dataDir, testAppGrant)
	writeAppCredential(t, cfg.dataDir, "b6b36f36-8c38-4b5d-8505-415f15deffbc")
	_, _, err := executeCLI(t, Deps{}, "app", "status")
	if ExitCode(err) != 2 || !strings.Contains(err.Error(), "--grant") {
		t.Fatalf("ambiguous: %v", err)
	}
	_, _, err = executeCLI(t, Deps{}, "app", "status", "--grant", "../bad")
	if ExitCode(err) != 2 {
		t.Fatalf("unsafe id: %v", err)
	}
}

func TestAppCredentialRejectsExpiredMalformedAndUnsafeFiles(t *testing.T) {
	for _, mode := range []string{"expired", "wrong-session", "malformed", "symlink", "hardlink", "permissions"} {
		t.Run(mode, func(t *testing.T) {
			cfg := setConfigEnv(t)
			t.Setenv("AO_SESSION_ID", "next.js-1")
			writeAppCredential(t, cfg.dataDir, testAppGrant)
			name := filepath.Join(cfg.dataDir, "diagnostics", "grants", testAppGrant+".json")
			raw, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "expired", "wrong-session":
				var value map[string]any
				if err := json.Unmarshal(raw, &value); err != nil {
					t.Fatal(err)
				}
				if mode == "expired" {
					value["expiresAt"] = time.Now().Add(-time.Minute)
				} else {
					value["sessionId"] = "other-1"
				}
				data, _ := json.Marshal(value)
				if err := os.WriteFile(name, data, 0600); err != nil {
					t.Fatal(err)
				}
			case "malformed":
				if err := os.WriteFile(name, []byte(`{"token":"secret"}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				target := filepath.Join(cfg.dataDir, "outside")
				if err := os.Rename(name, target); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, name); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
			case "hardlink":
				if err := os.Link(name, filepath.Join(cfg.dataDir, "outside")); err != nil {
					t.Skipf("hardlink unavailable: %v", err)
				}
			case "permissions":
				if goruntime.GOOS == "windows" {
					t.Skip("Unix mode check; Windows ACL test separate")
				}
				if err := os.Chmod(name, 0644); err != nil {
					t.Fatal(err)
				}
			}
			_, _, err = executeCLI(t, Deps{}, "app", "status")
			if err == nil || strings.Contains(err.Error(), "test-only-secret") {
				t.Fatalf("unsafe credential accepted/leaked: %v", err)
			}
		})
	}
}

func TestAppCapabilityNeverFollowsRedirect(t *testing.T) {
	cfg := setConfigEnv(t)
	t.Setenv("AO_SESSION_ID", "next.js-1")
	writeAppCredential(t, cfg.dataDir, testAppGrant)
	calls := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(http.StatusNoContent) }))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
	defer srv.Close()
	writeRunFileFor(t, cfg, srv)
	_, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "app", "status")
	if err == nil || calls != 0 {
		t.Fatalf("diagnostic request followed redirect: calls=%d err=%v", calls, err)
	}
}

func TestAppCaptureCancellationReachesDaemon(t *testing.T) {
	cfg := setConfigEnv(t)
	t.Setenv("AO_SESSION_ID", "next.js-1")
	writeAppCredential(t, cfg.dataDir, testAppGrant)
	received := make(chan struct{})
	cancelled := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/desktop/diagnostics/snapshots" {
			t.Error("unexpected diagnostic request")
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			return
		}
		close(received)
		<-r.Context().Done()
		close(cancelled)
	}))
	defer srv.Close()
	writeRunFileFor(t, cfg, srv)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := NewRootCommand(Deps{ProcessAlive: func(int) bool { return true }})
	cmd.SetArgs([]string{"app", "snapshot"})
	done := make(chan error, 1)
	go func() { done <- cmd.ExecuteContext(ctx) }()
	select {
	case <-received:
	case <-time.After(5 * time.Second):
		t.Fatal("capture not received")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled capture succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("CLI did not cancel")
	}
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not observe cancellation")
	}
}
