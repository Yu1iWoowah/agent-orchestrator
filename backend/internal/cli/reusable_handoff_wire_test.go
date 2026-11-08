//go:build linux

package cli

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	projectsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/project"
	projectdocs "github.com/aoagents/agent-orchestrator/backend/internal/service/projectdocuments"
)

// TestReusableHandoffWire is the only fixture producer/entry point. CLI samples
// pass through Cobra's real formatter; HTTP samples use the production DTOs.
// Nothing here connects to a daemon or provider, including the protocol suite.
func TestReusableHandoffWire(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal("RH wire tests require python3:", err)
	}
	cfg := setConfigEnv(t)
	dir := t.TempDir()
	stamp := "2026-11-08T00:00:00Z"
	at, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		t.Fatal(err)
	}
	session := func(id, kind, harness string) sessionDTO {
		return sessionDTO{ID: id, ProjectID: "agent-orchestrator", Kind: kind, Harness: harness,
			Mode: "chat", Status: "idle", Branch: "fixture", CreatedAt: at, UpdatedAt: at,
			Activity: sessionActivity{State: "idle", LastActivityAt: at}, PRs: []sessionPRDTO{}}
	}
	old := session("old", "orchestrator", "codex")
	apiSession := func(harness domain.AgentHarness) controllers.SessionResponse {
		return controllers.SessionResponse{Session: controllers.SessionView{Branch: "fixture", Session: domain.Session{
			SessionRecord: domain.SessionRecord{ID: "old", ProjectID: "agent-orchestrator", Kind: "orchestrator",
				Harness: harness, Mode: "chat", Activity: domain.Activity{State: "idle"}, CreatedAt: at, UpdatedAt: at}, Status: "idle"}}}
	}
	worker := session("worker", "worker", "claude-code")
	terminated := worker
	terminated.ID, terminated.IsTerminated = "terminated", true
	other := worker
	other.ID, other.ProjectID = "other", "other-project"
	newSession := old
	newSession.ID, newSession.DisplayName = "new", "RH3[wire] orchestrator"
	rows := []sessionDTO{old, worker, terminated, other, newSession}
	spawned := spawnResult{}
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
	accounts := accountList{Accounts: []accountInfo{}}
	for _, provider := range []string{"claude", "codex"} {
		accounts.Accounts = append(accounts.Accounts, accountInfo{ID: "acct", Provider: provider, Email: "fixture@example.invalid", SignedIn: true,
			Limits: &domain.AgentAccountLimits{Windows: []domain.AgentAccountLimitWindow{{ID: "window", UsedPercent: 10}}}})
	}
	write := func(name string, raw []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), raw, 0600); err != nil {
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var body any
		switch {
		case strings.HasPrefix(r.URL.Path, "/internal/"):
			return
		case r.URL.Path == "/api/v1/reports":
			body = reportAPIResponse{ID: "report"}
		case strings.HasSuffix(r.URL.Path, "/steer-or-send"):
			body = steerOrSendAPIResponse{Outcome: "sent", TurnID: "turn", State: "queued"}
		case r.URL.Path == "/api/v1/sessions" && r.Method == http.MethodPost:
			body = spawned
		case r.URL.Path == "/api/v1/sessions":
			body = sessionListResponse{Sessions: rows}
		case strings.HasSuffix(r.URL.Path, "/prs"):
			body = sessionPRSummaryResponse{}
		case strings.HasSuffix(r.URL.Path, "/conversation"):
			snap := conversation(strings.Contains(r.URL.Path, "active"))
			if r.URL.Query().Get("beforeSequence") != "" {
				snap.Messages, snap.Activities = snap.Messages[:1], nil
			} else if r.URL.Query().Get("limit") == "1" {
				snap.Messages, snap.HasMoreBefore = nil, true
			}
			body = snap
		case strings.HasSuffix(r.URL.Path, "/resume-agent"):
			body = resumeAgentResponse{OK: true, SessionID: "old", ResumeMode: "native", Session: old}
		case strings.HasSuffix(r.URL.Path, "/exit-agent"):
			body = exitAgentResponse{OK: true, SessionID: "old", Session: old}
		case strings.HasSuffix(r.URL.Path, "/kill"):
			body = killSessionResponse{SessionID: "old", Freed: !strings.Contains(r.URL.Path, "preserved"), CleanupPending: strings.Contains(r.URL.Path, "pending")}
		case strings.Contains(r.URL.Path, "/documents"):
			switch {
			case strings.HasSuffix(r.URL.Path, "/lease"):
				body = projectDocumentLeaseResponse{Lease: projectDocumentGrant{Epoch: 1, Token: "test-only"}}
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
	}))
	defer srv.Close()
	writeRunFileFor(t, cfg, srv)
	t.Setenv("AO_SESSION_ID", "old")
	t.Setenv("AO_BROWSER_CAPABILITY", "test-only")
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
		out, stderr, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, args...)
		if err != nil {
			t.Fatalf("%s: %v (%s)", name, err, stderr)
		}
		write(name+".json", []byte(out))
	}
	// Managed write also goes through Cobra, including its lease request.
	draft := filepath.Join(dir, "draft.md")
	write("draft.md", []byte(content))
	out, stderr, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "project", "documents", "write", "handoff.md",
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
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	tests := filepath.Join(repo, "tools", "reusable-handoff", "tests")
	cmd := exec.Command(python, "-I", "-B", "-m", "unittest", "discover", "-v", "-s", tests)
	cmd.Env = append(os.Environ(), "RH_WIRE_FIXTURES="+dir)
	result, err := cmd.CombinedOutput()
	t.Logf("fresh Cobra/DTO fixtures → Python (no test-result cache in focused command):\n%s", result)
	if err != nil {
		t.Fatal("RH Python suite:", err)
	}
}
