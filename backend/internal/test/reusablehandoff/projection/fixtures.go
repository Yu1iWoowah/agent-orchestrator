//go:build linux

// Package reusablehandoff is imported only by tests; it has no product call sites.
package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/daemonmeta"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/process"
	projectsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/project"
	projectdocs "github.com/aoagents/agent-orchestrator/backend/internal/service/projectdocuments"
)

func invoke(bin string, env []string, args ...string) (string, string, error) {
	cmd := process.Command(bin, args...)
	cmd.Env = env
	var out, stderr strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	err := cmd.Run()
	return out.String(), stderr.String(), err
}

// daemonIdentity answers the CLI's loopback identity probes for this fixture
// process, matching the PID that cliEnvironment writes to the run-file.
func daemonIdentity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status := map[string]string{"/healthz": "ok", "/readyz": "ready"}[r.URL.Path]
		if status == "" || r.Method != http.MethodGet {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": status, "service": daemonmeta.ServiceName, "pid": os.Getpid()})
	})
}

func cliEnvironment(t *checks, dir string, srv *httptest.Server) []string {
	t.Helper()
	run := filepath.Join(dir, "run.json")
	address, ok := srv.Listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatal("fixture listener is not TCP")
	}
	raw, err := json.Marshal(map[string]any{"pid": os.Getpid(), "port": address.Port})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(run, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	// subprocess environment is deliberately reconstructed, with no inherited AO/provider state.
	env := []string{}
	for _, key := range []string{"PATH", "HOME", "TMPDIR", "TMUX_TMPDIR", "GOMODCACHE", "GOCACHE", "GOMAXPROCS", "GOFLAGS", "LANG", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_NOSYSTEM"} {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	return append(env, "AO_RUN_FILE="+run, "AO_DATA_DIR="+filepath.Join(dir, "data"))
}

func Fixtures(t *checks, bin, dir string) {
	t.Helper()
	stamp := "2026-11-08T00:00:00Z"
	at, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		t.Fatal(err)
	}
	session := func(id, kind, harness string) controllers.SessionView {
		return controllers.SessionView{Branch: "fixture", Session: domain.Session{SessionRecord: domain.SessionRecord{
			ID: domain.SessionID(id), ProjectID: "agent-orchestrator", Kind: domain.SessionKind(kind), Harness: domain.AgentHarness(harness),
			Mode: "chat", Activity: domain.Activity{State: "idle"}, CreatedAt: at, UpdatedAt: at}, Status: "idle"}}
	}
	old := session("old", "orchestrator", "codex")
	apiSession := func(harness domain.AgentHarness) controllers.SessionResponse {
		return controllers.SessionResponse{Session: session("old", "orchestrator", string(harness))}
	}
	worker := session("worker", "worker", "claude-code")
	terminated := worker
	terminated.ID, terminated.IsTerminated = "terminated", true
	other := worker
	other.ID, other.ProjectID = "other", "other-project"
	newSession := old
	newSession.ID, newSession.DisplayName = "new", "RH3[wire] orchestrator"
	rows := []controllers.SessionView{old, worker, terminated, other, newSession}
	spawned := controllers.SpawnSessionResponse{}
	spawned.Session.ID, spawned.Session.Status = "new", "idle"
	settings := controllers.ConversationTurnSettingsPayload{Model: "model", ReasoningEffort: "high", ApprovalMode: "bypass-permissions"}
	conversation := func(active bool) controllers.ConversationSnapshotResponse {
		turn := controllers.ConversationTurnResponse{ID: "preparing", State: "completed", RequestedAt: stamp,
			CompletedAt: &stamp, ProviderTurnID: "provider-turn", AgentSettings: &domain.AgentSettingsTurnSnapshot{
				State: "known", Settings: &domain.AgentSettings{Model: "model", Effort: "high"}}}
		if active {
			turn.State, turn.CompletedAt = "running", nil
		}
		return controllers.ConversationSnapshotResponse{ConversationID: "conv", ActiveBranchID: "branch", SessionID: "old",
			Mode: "chat", Controller: "ready", LatestSequence: 3, OldestSequence: 1, Settings: settings,
			Turns: []controllers.ConversationTurnResponse{turn},
			Messages: []controllers.ConversationMessageResponse{
				{Kind: "message", ID: "human", TurnID: "preparing", Sequence: 1, Revision: 1, Role: "user", Origin: "human", Text: "handoff", CreatedAt: stamp},
				{Kind: "message", ID: "workspace", TurnID: "preparing", Sequence: 2, Revision: 1, Role: "assistant", Origin: "provider", Text: "old /workspace", Streaming: active, CreatedAt: stamp}},
			Activities: []controllers.ConversationActivityResponse{{Kind: "activity", ID: "tool", TurnID: "preparing", Sequence: 3,
				ActivityKind: "command", Status: "completed", Summary: "read", Detail: map[string]any{"command": "pwd", "exitCode": 0}, CreatedAt: stamp}}}
	}
	project := controllers.GetProjectResponse{Status: "ok", Project: controllers.ProjectOrDegraded{Project: &projectsvc.Project{
		ID: "agent-orchestrator", Path: "/repository", Config: &domain.ProjectConfig{}}}}
	documents := controllers.ProjectDocumentsInfoResponse{Documents: projectdocs.Info{
		SharedDir: "/data/projects/agent-orchestrator/orchestrator/shared", DocumentPath: "handoff.md",
		Files: []domain.ProjectDocumentEntry{}, Lease: &projectdocs.LeaseView{SessionID: "old", Epoch: 1}}}
	content := "handoff_run_id: wire\n"
	managed := controllers.ProjectDocumentFileResponse{File: projectdocs.File{Path: "handoff.md", Content: content,
		Hash: fmt.Sprintf("%x", sha256.Sum256([]byte(content))), Size: int64(len(content))}}
	accounts := controllers.AgentAccountsResponse{Accounts: []controllers.AgentAccountResponse{}}
	for _, provider := range []string{"claude", "codex"} {
		accounts.Accounts = append(accounts.Accounts, controllers.AgentAccountResponse{ID: "acct", Provider: domain.AgentAccountProvider(provider), Email: "fixture@example.invalid", SignedIn: true,
			Limits: &domain.AgentAccountLimits{Windows: []domain.AgentAccountLimitWindow{{ID: "window", UsedPercent: 10}}}})
	}
	write := func(name string, raw []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	marshal := func(value any) []byte {
		t.Helper()
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	// Routes are immutable: parallel HTTP requests never race fixture selection.
	srv := httptest.NewServer(daemonIdentity(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var body any
		switch {
		case strings.HasPrefix(r.URL.Path, "/internal/"):
			return
		case r.URL.Path == "/api/v1/reports":
			body = controllers.CreateReportResponse{ID: "report"}
		case strings.HasSuffix(r.URL.Path, "/steer-or-send"):
			body = controllers.SteerOrSendConversationResponse{Outcome: "sent", TurnID: "turn", State: "queued"}
		case r.URL.Path == "/api/v1/sessions" && r.Method == http.MethodPost:
			body = spawned
		case r.URL.Path == "/api/v1/sessions":
			body = controllers.ListSessionsResponse{Sessions: rows}
		case strings.HasSuffix(r.URL.Path, "/prs"):
			body = controllers.ListSessionPRsResponse{}
		case strings.HasSuffix(r.URL.Path, "/conversation"):
			snap := conversation(strings.Contains(r.URL.Path, "active"))
			if r.URL.Query().Get("beforeSequence") != "" {
				snap.Messages, snap.Activities = snap.Messages[:1], nil
			} else if r.URL.Query().Get("limit") == "1" {
				snap.Messages, snap.HasMoreBefore = nil, true
			}
			body = snap
		case strings.HasSuffix(r.URL.Path, "/resume-agent"):
			body = controllers.ResumeAgentResponse{OK: true, SessionID: "old", ResumeMode: "native", Session: old}
		case strings.HasSuffix(r.URL.Path, "/exit-agent"):
			body = controllers.ExitAgentResponse{OK: true, SessionID: "old", Session: old}
		case strings.HasSuffix(r.URL.Path, "/kill"):
			body = controllers.KillSessionResponse{SessionID: "old", Freed: !strings.Contains(r.URL.Path, "preserved"), CleanupPending: strings.Contains(r.URL.Path, "pending")}
		case strings.Contains(r.URL.Path, "/documents"):
			switch {
			case strings.HasSuffix(r.URL.Path, "/lease"):
				body = controllers.ProjectDocumentLeaseResponse{Lease: projectdocs.Grant{Epoch: 1, Token: "test-only"}}
			case strings.HasSuffix(r.URL.Path, "/file"):
				body = managed
			default:
				body = documents
			}
		case r.URL.Path == "/api/v1/agents/accounts":
			body = accounts
		case strings.HasPrefix(r.URL.Path, "/api/v1/projects/"):
			body = project
		case strings.HasPrefix(r.URL.Path, "/api/v1/sessions/"):
			harness := domain.HarnessCodex
			if strings.Contains(r.URL.Path, "claude") {
				harness = domain.HarnessClaudeCode
			}
			body = apiSession(harness)
		default:
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(marshal(body))
	})))
	defer srv.Close()
	env := cliEnvironment(t, dir, srv)
	executeCLI := func(args ...string) (string, string, error) { return invoke(bin, env, args...) }
	env = append(env, "AO_SESSION_ID=old", "AO_BROWSER_CAPABILITY=test-only")
	commands := map[string][]string{
		"help":            {"spawn", "--help"},
		"send":            {"send", "--session", "worker", "--steer", "--client-message-id", "wire", "--message", "fixture"},
		"report":          {"report", "--needs-input", "--note", "fixture"},
		"spawn":           {"spawn", "--project", "agent-orchestrator", "--kind", "orchestrator", "--agent", "codex", "--account", "acct", "--mode", "chat", "--name", "wire"},
		"list":            {"session", "ls", "--all", "--include-terminated", "--project", "agent-orchestrator", "--json"},
		"detail":          {"session", "get", "old", "--json"},
		"snapshot":        {"session", "get", "old", "--transcript", "100", "--cap", "0", "--json"},
		"claude-snapshot": {"session", "get", "claude", "--transcript", "100", "--cap", "0", "--json"},
		"active":          {"session", "get", "active", "--transcript", "100", "--cap", "0", "--json"},
		"page":            {"session", "get", "old", "--transcript", "1", "--cap", "0", "--json"},
		"before":          {"session", "get", "old", "--transcript", "100", "--cap", "0", "--before", "3", "--json"},
		"documents":       {"project", "documents", "info", "--project", "agent-orchestrator", "--json"},
		"accounts":        {"account", "ls", "--json"},
		"resume":          {"session", "resume-agent", "old", "--project", "agent-orchestrator", "--json"},
		"exit":            {"session", "exit-agent", "old", "--project", "agent-orchestrator"},
		"kill":            {"session", "kill", "old", "--project", "agent-orchestrator"},
		"kill-preserved":  {"session", "kill", "preserved", "--project", "agent-orchestrator"},
		"kill-pending":    {"session", "kill", "pending", "--project", "agent-orchestrator"},
	}
	for name, args := range commands {
		out, stderr, err := executeCLI(args...)
		if err != nil {
			t.Fatalf("%s: %v (%s)", name, err, stderr)
		}
		write(name+".json", []byte(out))
	}
	// Managed write also goes through Cobra, including its lease request.
	draft := filepath.Join(dir, "draft.md")
	write("draft.md", []byte(content))
	out, stderr, err := executeCLI("project", "documents", "write", "handoff.md",
		"--project", "agent-orchestrator", "--file", draft, "--expected-hash", "missing", "--json")
	if err != nil {
		t.Fatalf("managed: %v (%s)", err, stderr)
	}
	write("managed.json", []byte(out))
	write("project.json", marshal(project))
	write("degraded.json", marshal(controllers.GetProjectResponse{Status: "degraded", Project: controllers.ProjectOrDegraded{
		Degraded: &projectsvc.Degraded{ID: "agent-orchestrator", ResolveError: "fixture"}}}))
	for _, provider := range []domain.AgentAccountProvider{"claude", "codex"} {
		write(string(provider)+"-models.json", marshal(ports.AgentModelCatalog{
			AccountBinding: &domain.ResolvedAgentAccount{AccountID: "acct", Provider: provider},
			Models:         []ports.AgentModelInfo{{ID: "model", Efforts: []string{"high"}}}}))
		binding := controllers.SessionAgentAccountsResponse{Bindings: map[domain.AgentAccountProvider]domain.AgentAccountBindingView{
			provider: {AccountID: "acct", Provider: provider, Available: true}}}
		if provider == "claude" {
			binding.Claude = "acct"
		} else {
			binding.Codex = "acct"
		}
		write(string(provider)+"-binding.json", marshal(binding))
	}
	write("conversation.json", marshal(conversation(false)))
	write("http-detail.json", marshal(apiSession(domain.HarnessCodex)))
}
