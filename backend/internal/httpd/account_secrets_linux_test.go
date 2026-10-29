package httpd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agentaccount"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/codexappserver"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/agentaccounts"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/shellterm"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

type executableAccountTerminal struct{}

func (executableAccountTerminal) OpenCommandTerminal(ctx context.Context, in shellterm.OpenCommandTerminalInput) (shellterm.ShellTerminal, error) {
	c := exec.CommandContext(ctx, in.Argv[0], in.Argv[1:]...)
	c.Dir = in.WorkingDir
	c.Env = os.Environ()
	for k, v := range in.Env {
		c.Env = append(c.Env, k+"="+v)
	}
	c.Stdout = io.Discard
	c.Stderr = io.Discard
	return shellterm.ShellTerminal{HandleID: "fake-terminal"}, c.Run()
}
func (executableAccountTerminal) CloseShellTerminal(context.Context, string) error { return nil }
func TestProviderSettingSecretDoesNotReachSinks(t *testing.T) {
	ctx := context.Background()
	for _, key := range []string{"OPENAI_API_KEY", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN"} {
		t.Setenv(key, "fake-ambient-login-credential")
	}
	root := filepath.Join(t.TempDir(), "accounts")
	device := t.TempDir()
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	bin := filepath.Join(t.TempDir(), "fake-codex")
	script := `#!/usr/bin/python3
import os,sys,json
h=os.environ['CODEX_HOME']
if h and any(os.environ.get(k) for k in ('OPENAI_API_KEY','CODEX_API_KEY','CODEX_ACCESS_TOKEN')): sys.exit(42)
if 'login' in sys.argv:
 p=os.path.join(h,'auth.json')
 fd=os.open(p,os.O_WRONLY|os.O_CREAT|os.O_TRUNC,0o600)
 os.write(fd,b'{}');os.close(fd)
 sys.exit(0)
for line in sys.stdin:
 q=json.loads(line)
 if 'id' not in q: continue
 result={}
 p=os.path.join(h,'settings.json')
 if os.path.exists(p):
  settings=json.load(open(p))
  if q['method']==settings['failure_method']:
   secret=settings['env']['CODEX_ACCESS_TOKEN']
   print(json.dumps({'id':q['id'],'error':{'code':-32603,'message':'policy rejected configured credential: '+secret+' in '+p}}),flush=True)
   continue
 if q['method']=='account/read':
  result={'account':{'type':'chatgpt','email':os.path.basename(h)+'@example.invalid'},'requiresOpenaiAuth':True}
 if q['method']=='account/rateLimits/read':result={'rateLimits':None}
 print(json.dumps({'id':q['id'],'result':result}),flush=True)
`
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	resolve := func(context.Context) (string, error) { return bin, nil }
	provider := &agentaccount.Codex{Binary: resolve, DeviceHome: device, Accounts: codexappserver.NewAccountFactoryWithResolver(resolve, logger)}
	dbDir := t.TempDir()
	store := sqlitetest.MustOpenAt(t, dbDir)
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "safe-replay-project", Path: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	// Sessions have CDC triggers; project registration alone does not emit one.
	now := time.Now().UTC()
	if _, err := store.CreateSession(ctx, domain.SessionRecord{ProjectID: "safe-replay-project", Kind: domain.KindWorker, Harness: domain.HarnessCodex, Activity: domain.Activity{State: domain.ActivityIdle, LastActivityAt: now}, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	svc := agentaccounts.New(store, []ports.AgentAccountProviderCLI{provider}, root, executableAccountTerminal{}, logger)
	var ids []string
	for range 2 {
		start, err := svc.StartLogin(ctx, domain.AgentAccountProviderCodex)
		if err != nil {
			t.Fatal(err)
		}
		status, view, err := svc.VerifyLogin(ctx, start.OperationID)
		if err != nil || status != agentaccounts.LoginComplete || view == nil {
			t.Fatalf("fake login failed: status=%q err=%v", status, err)
		}
		ids = append(ids, start.OperationID)
	}
	// The fake provider reflects a config error containing only this synthetic marker.
	marker := "fake-provider-secret-c7e6"
	settings := filepath.Join(root, "codex", ids[0], "settings.json")
	c := &controllers.AgentAccountsController{Svc: svc}
	router := chi.NewRouter()
	c.Register(router)
	for _, method := range []string{"account/read", "account/rateLimits/read"} {
		data, err := json.Marshal(map[string]any{"env": map[string]string{"CODEX_ACCESS_TOKEN": marker}, "failure_method": method})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(settings, data, 0o600); err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/agents/accounts?refresh=true", nil))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "account_provider_unavailable") {
			t.Error("provider error did not produce a safe problem category")
		}
		if strings.Contains(rec.Body.String(), marker) || strings.Contains(rec.Body.String(), settings) {
			t.Error("provider credential or path reached API problem")
		}
	}
	rows, err := store.ListAgentAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(rows)
	if bytes.Contains(data, []byte(marker)) {
		t.Error("credential marker reached durable account rows")
	}
	events, err := store.EventsAfter(ctx, 0, 10000)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 {
		t.Fatal("fixture CDC source is empty")
	}
	data, _ = json.Marshal(events)
	if bytes.Contains(data, []byte(marker)) {
		t.Error("credential marker reached CDC/SSE source")
	}
	if !strings.Contains(logs.String(), "account_provider_unavailable") {
		t.Error("fixture did not capture safe category log")
	}
	if strings.Contains(logs.String(), marker) {
		t.Error("credential marker reached captured logger")
	}
	sseCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	sse := httptest.NewRecorder()
	(&EventsController{Source: store, Live: &fakeEventSubscriber{}}).stream(sse, httptest.NewRequest(http.MethodGet, "/events?after=0", nil).WithContext(sseCtx))
	if sse.Code != http.StatusOK || !strings.Contains(sse.Body.String(), "event:") {
		t.Fatal("fixture did not produce SSE replay")
	}
	if strings.Contains(sse.Body.String(), marker) {
		t.Error("credential marker reached SSE frame")
	}
	if err := filepath.WalkDir(dbDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if bytes.Contains(data, []byte(marker)) {
				t.Error("credential marker reached SQLite or WAL bytes")
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(root, "codex", ids[1])
	if err := filepath.WalkDir(other, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if bytes.Contains(data, []byte(marker)) {
				t.Error("credential marker reached another profile")
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
