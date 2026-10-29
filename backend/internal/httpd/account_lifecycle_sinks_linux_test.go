package httpd

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
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
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

func TestLifecycleSecretsStayOutOfDurableAndExportSinks(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "accounts")
	device := t.TempDir()
	dbDir := t.TempDir()
	for _, key := range []string{"OPENAI_API_KEY", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN"} {
		t.Setenv(key, "fake-ambient-lifecycle-secret")
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	bin := filepath.Join(t.TempDir(), "fake-codex")
	script := `#!/usr/bin/python3
import os,sys,json
h=os.environ['CODEX_HOME']
if h and any(os.environ.get(k) for k in ('OPENAI_API_KEY','CODEX_API_KEY','CODEX_ACCESS_TOKEN')): sys.exit(42)
if 'login' in sys.argv:
 fd=os.open(os.path.join(h,'auth.json'),os.O_WRONLY|os.O_CREAT|os.O_TRUNC,0o600);os.write(fd,b'{}');os.close(fd);sys.exit(0)
for line in sys.stdin:
 q=json.loads(line)
 if 'id' not in q: continue
 result={}
 p=os.path.join(h,'settings.json')
 if os.path.exists(p):
  s=json.load(open(p))
  if q['method']==s['failure_method']:
   open(os.path.join(h,'rpc-arrived'),'w').close()
   print(json.dumps({'id':q['id'],'error':{'code':-32603,'message':'credential rejected '+s['env']['CODEX_ACCESS_TOKEN']+' at '+p}}),flush=True);continue
 if q['method']=='account/read': result={'account':{'type':'chatgpt','email':os.path.basename(h)+'@example.invalid'} if os.path.exists(os.path.join(h,'auth.json')) else None,'requiresOpenaiAuth':True}
 if q['method']=='account/rateLimits/read': result={'rateLimits':None}
 print(json.dumps({'id':q['id'],'result':result}),flush=True)
`
	if e := os.WriteFile(bin, []byte(script), 0700); e != nil {
		t.Fatal(e)
	}
	resolve := func(context.Context) (string, error) { return bin, nil }
	p := &agentaccount.Codex{Binary: resolve, DeviceHome: device, Accounts: codexappserver.NewAccountFactoryWithResolver(resolve, logger)}
	db := sqlitetest.MustOpenAt(t, dbDir)
	if e := db.UpsertProject(ctx, domain.ProjectRecord{ID: "fake-lifecycle-project", Path: t.TempDir()}); e != nil {
		t.Fatal(e)
	}
	svc := agentaccounts.New(db, []ports.AgentAccountProviderCLI{p}, root, executableAccountTerminal{}, logger)
	var ids []string
	for range 2 {
		start, e := svc.StartLogin(ctx, domain.AgentAccountProviderCodex)
		if e != nil {
			t.Fatal(e)
		}
		status, view, e := svc.VerifyLogin(ctx, start.OperationID)
		if e != nil || status != agentaccounts.LoginComplete || view == nil {
			t.Fatalf("fake login failed: %s %v", status, e)
		}
		ids = append(ids, start.OperationID)
	}
	marker := "fake-lifecycle-credential-83af"
	settings := filepath.Join(root, "codex", ids[0], "settings.json")
	setFailure := func(method string) {
		t.Helper()
		data, _ := json.Marshal(map[string]any{"failure_method": method, "env": map[string]string{"CODEX_ACCESS_TOKEN": marker}})
		if e := os.WriteFile(settings, data, 0600); e != nil {
			t.Fatal(e)
		}
	}
	sink := &captureSink{}
	var captures []string
	router := chi.NewRouter()
	router.Use(requestLoggerWithCapture(logger, sink, func(_ context.Context, e error, _ map[string]string, _ string) {
		captures = append(captures, e.Error())
	}))
	(&controllers.AgentAccountsController{Svc: svc}).Register(router)
	var responses bytes.Buffer
	request := func(method, path, body string, intended int) string {
		t.Helper()
		r := httptest.NewRecorder()
		router.ServeHTTP(r, httptest.NewRequest(method, path, strings.NewReader(body)))
		responses.Write(r.Body.Bytes())
		if r.Code != intended {
			t.Fatalf("%s %s: HTTP %d expected %d", method, path, r.Code, intended)
		}

		return r.Body.String()
	}
	setFailure("account/rateLimitResetCredit/consume")
	request(http.MethodPost, "/agents/accounts/"+ids[0]+"/reset-credit", `{"idempotencyKey":"fake-once"}`, 500)
	setFailure("account/logout")
	logoutResponse := request(http.MethodPost, "/agents/accounts/"+ids[0]+"/logout", "", 409)
	request(http.MethodPost, "/agents/accounts/"+ids[0]+"/reset-credit", `{"idempotencyKey":"blocked-once"}`, 409)
	ops, e := db.ListAgentAccountOperations(ctx)
	if e != nil {
		t.Fatal(e)
	}
	var failed domain.AgentAccountOperation
	for _, o := range ops {
		if o.Action == "logout" {
			failed = o
		}
	}
	if failed.ID == "" || failed.Phase == "complete" || failed.ReasonCode != "account_cleanup_unconfirmed" {
		t.Fatal("failed native logout did not retain retryable progress")
	}
	if !strings.Contains(logoutResponse, failed.ID) {
		t.Fatal("unconfirmed response cannot be followed to its durable operation")
	}
	progress := request(http.MethodGet, "/agents/accounts/operations/"+failed.ID, "", 200)
	if strings.Contains(progress, `"outcome"`) {
		t.Fatal("unconfirmed logout API claims a completed outcome")
	}
	request(http.MethodGet, "/agents/accounts?refresh=true", "", 200)
	if _, e := os.Stat(filepath.Join(root, "codex", ids[0], "rpc-arrived")); e != nil {
		t.Fatal("secret-reflecting RPC was not reached")
	}
	if len(sink.events) == 0 || len(captures) == 0 {
		t.Fatal("fake Cloud telemetry/capture sinks were not exercised")
	}
	sseCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	sse := httptest.NewRecorder()
	(&EventsController{Source: db, Live: &fakeEventSubscriber{}}).stream(sse, httptest.NewRequest(http.MethodGet, "/events?after=0", nil).WithContext(sseCtx))
	if !strings.Contains(sse.Body.String(), `"kind":"agent_accounts"`) {
		t.Fatal("no lifecycle CDC reached SSE replay")
	}
	cloudData, _ := json.Marshal(sink.events)
	journalData, _ := json.Marshal(ops)
	captureData, _ := json.Marshal(captures)
	// A local fake export sink consumes exactly the real middleware telemetry
	// and durable operation projections, never native stdout/config bytes.
	export, _ := json.Marshal(struct {
		Telemetry  []ports.TelemetryEvent
		Operations []domain.AgentAccountOperation
	}{sink.events, ops})
	artifact := filepath.Join(t.TempDir(), "fake-export.json")
	if e := os.WriteFile(artifact, export, 0600); e != nil {
		t.Fatal(e)
	}
	artifactData, e := os.ReadFile(artifact)
	if e != nil {
		t.Fatal(e)
	}
	for name, data := range map[string][]byte{"API": responses.Bytes(), "SSE": sse.Body.Bytes(), "log": logs.Bytes(), "journal": journalData, "fake Cloud": cloudData, "capture": captureData, "fake artifact export": artifactData} {
		if bytes.Contains(data, []byte(marker)) || bytes.Contains(data, []byte("fake-ambient-lifecycle-secret")) {
			t.Errorf("credential marker reached %s", name)
		}
	}
	if !strings.Contains(responses.String(), "account_cleanup_unconfirmed") {
		t.Fatal("API did not expose safe retry category")
	}
	for _, dir := range []string{dbDir, filepath.Join(root, "codex", ids[1]), device} {
		if e := filepath.WalkDir(dir, func(path string, d os.DirEntry, e error) error {
			if e != nil {
				return e
			}
			if d.Type().IsRegular() {
				data, e := os.ReadFile(path)
				if e != nil {
					return e
				}
				if bytes.Contains(data, []byte(marker)) {
					t.Error("marker crossed DB/other-profile/device boundary")
				}
			}
			return nil
		}); e != nil {
			t.Fatal(e)
		}
	}
}
