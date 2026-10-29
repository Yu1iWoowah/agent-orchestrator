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

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agentaccount"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/codexappserver"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/agentaccounts"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

func TestResetCreditRPCSecretDoesNotReachObservability(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "accounts")
	device := t.TempDir()
	var debugLogs, accessLogs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&debugLogs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	bin := filepath.Join(t.TempDir(), "fake-codex")
	script := `#!/usr/bin/python3
import os,sys,json
h=os.environ['CODEX_HOME']
if 'login' in sys.argv:
 p=os.path.join(h,'auth.json')
 fd=os.open(p,os.O_WRONLY|os.O_CREAT|os.O_TRUNC,0o600)
 os.write(fd,b'{}');os.close(fd)
 sys.exit(0)
for line in sys.stdin:
 q=json.loads(line)
 if 'id' not in q: continue
 result={}
 if q['method']=='account/rateLimitResetCredit/consume':
  open(os.path.join(h,'reset-rpc-arrived'),'w').close()
  secret=json.load(open(os.path.join(h,'settings.json')))['env']['CODEX_ACCESS_TOKEN']
  print(json.dumps({'id':q['id'],'error':{'code':-32603,'message':'policy rejected configured credential: '+secret}}),flush=True)
  continue
 if q['method']=='account/read':
  p=os.path.join(h,'settings.json')
  if os.path.exists(p):
   secret=json.load(open(p))['env']['CODEX_ACCESS_TOKEN']
   print(json.dumps({'id':q['id'],'error':{'code':-32603,'message':'policy rejected configured credential: '+secret}}),flush=True)
   continue
  result={'account':{'type':'chatgpt','email':os.path.basename(h)+'@example.invalid'},'requiresOpenaiAuth':True}
 if q['method']=='account/rateLimits/read':result={'rateLimits':None}
 print(json.dumps({'id':q['id'],'result':result}),flush=True)
`
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	resolve := func(context.Context) (string, error) { return bin, nil }
	provider := &agentaccount.Codex{Binary: resolve, DeviceHome: device, Accounts: codexappserver.NewAccountFactoryWithResolver(resolve, logger)}
	store := sqlitetest.MustOpen(t)
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
	marker := "fake-reset-provider-secret-73ae"
	settings := filepath.Join(root, "codex", ids[0], "settings.json")
	if err := os.WriteFile(settings, []byte(`{"env":{"CODEX_ACCESS_TOKEN":"`+marker+`"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	var captured []error
	c := &controllers.AgentAccountsController{Svc: svc}
	router := chi.NewRouter()
	router.Use(requestLoggerWithCapture(slog.New(slog.NewTextHandler(&accessLogs, nil)), nil, func(_ context.Context, err error, _ map[string]string, _ string) { captured = append(captured, err) }))
	c.Register(router)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/agents/accounts/"+ids[0]+"/reset-credit", strings.NewReader(`{"idempotencyKey":"fake-reset-once"}`)))
	if rec.Code != 500 {
		t.Fatalf("fixture did not reach RPC failure: HTTP %d", rec.Code)
	}
	if len(captured) != 1 {
		t.Fatal("fixture did not reach request error capture")
	}
	if _, err := os.Stat(filepath.Join(root, "codex", ids[0], "reset-rpc-arrived")); err != nil {
		t.Fatal("fixture did not reach native reset RPC")
	}
	if !strings.Contains(captured[0].Error(), "account_provider_unavailable") {
		t.Error("capture did not receive safe provider category")
	}
	if !strings.Contains(accessLogs.String(), "account_provider_unavailable") {
		t.Error("access logger did not receive safe provider category")
	}
	if !strings.Contains(debugLogs.String(), "account_provider_unavailable") {
		t.Error("debug logger did not receive safe provider category")
	}
	if strings.Contains(rec.Body.String(), marker) {
		t.Error("settings credential marker reached API account.problem through native RPC error")
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
	data, _ = json.Marshal(events)
	if bytes.Contains(data, []byte(marker)) {
		t.Error("credential marker reached CDC/SSE source")
	}
	if strings.Contains(accessLogs.String(), marker) {
		t.Error("reset-credit RPC credential marker reached HTTP access logger")
	}
	if strings.Contains(debugLogs.String(), marker) {
		t.Error("reset-credit RPC credential marker reached Debug logger")
	}
	if strings.Contains(captured[0].Error(), marker) {
		t.Error("reset-credit RPC credential marker reached HTTP error capture callback")
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
