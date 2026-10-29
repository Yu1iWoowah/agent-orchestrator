package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agentaccount"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/reviewer"
	telemetryadapter "github.com/aoagents/agent-orchestrator/backend/internal/adapters/telemetry"
	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/lifecycle"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	agentsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/agent"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/agentaccounts"
	reviewsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/review"
	sessionmanager "github.com/aoagents/agent-orchestrator/backend/internal/session_manager"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

type bindingRuntime struct {
	selectableRuntime
	mu       sync.Mutex
	launches []ports.RuntimeConfig
	alive    map[string]bool
}

func (r *bindingRuntime) Create(_ context.Context, c ports.RuntimeConfig) (ports.RuntimeHandle, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c.Env = maps.Clone(c.Env)
	r.launches = append(r.launches, c)
	id := fmt.Sprintf("fake-%d", len(r.launches))
	if r.alive == nil {
		r.alive = map[string]bool{}
	}
	r.alive[id] = true
	return ports.RuntimeHandle{ID: id}, nil
}
func (r *bindingRuntime) snapshot() []ports.RuntimeConfig {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ports.RuntimeConfig(nil), r.launches...)
}

func (r *bindingRuntime) IsAlive(_ context.Context, h ports.RuntimeHandle) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.alive[h.ID], nil
}
func (r *bindingRuntime) IsChildAlive(ctx context.Context, h ports.RuntimeHandle) (bool, error) {
	return r.IsAlive(ctx, h)
}
func (r *bindingRuntime) IsExactSupervisedProcessAlive(ctx context.Context, h ports.RuntimeHandle, _ ports.SupervisedProcessRef) (bool, error) {
	return r.IsAlive(ctx, h)
}
func (r *bindingRuntime) ProbeFencedRuntime(ctx context.Context, ref ports.FencedRuntimeRef) ports.FencedProbeResult {
	alive, _ := r.IsAlive(ctx, ref.Handle)
	if alive {
		return ports.FencedProbeResult{Liveness: ports.FencedAlive, Reason: ports.FencedReasonExactMatch}
	}
	return ports.FencedProbeResult{Liveness: ports.FencedDead, Reason: ports.FencedReasonExactAbsent}
}

type bindingProvider struct {
	ports.AgentAccountProviderCLI
	mu    sync.Mutex
	homes []string
}

func (p *bindingProvider) Identity(_ context.Context, home string) (domain.AgentAccountIdentityCheck, error) {
	p.mu.Lock()
	p.homes = append(p.homes, home)
	p.mu.Unlock()
	if home == "" {
		return domain.AgentAccountIdentityCheck{}, fmt.Errorf("device probe forbidden")
	}
	return domain.AgentAccountIdentityCheck{SignedIn: true, LoggedIn: true, Email: filepath.Base(home) + "@example.invalid"}, nil
}
func (*bindingProvider) Limits(context.Context, string) (domain.AgentAccountLimits, error) {
	return domain.AgentAccountLimits{}, nil
}

type bindingModels struct {
	mu       sync.Mutex
	requests []ports.AgentModelDiscoveryRequest
}

func (d *bindingModels) Discover(_ context.Context, r ports.AgentModelDiscoveryRequest) (ports.AgentModelCatalog, error) {
	d.mu.Lock()
	d.requests = append(d.requests, r)
	d.mu.Unlock()
	return ports.AgentModelCatalog{AgentID: r.AgentID, Source: "fake", SelectionMode: ports.ModelSelectionCatalog, Models: []ports.AgentModelInfo{{ID: "native", Label: "Native", IsDefault: true, Efforts: []string{"high"}}}}, nil
}
func (*bindingModels) CatalogFingerprint(_ context.Context, r ports.AgentModelDiscoveryRequest) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(r.Env["CODEX_HOME"]+r.Env["CLAUDE_CONFIG_DIR"])))
}
func (*bindingModels) Manual(id string) ports.AgentModelCatalog {
	return ports.AgentModelCatalog{AgentID: id}
}

// Exercise the daemon's real service wiring with provider/runtime substitutes.
// The marker is deliberately fake; no user's provider home is opened.
func TestWiringConcurrentAccountBindingsSurviveDefaultsAndRestore(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st := sqlitetest.MustOpen(t)
	data := t.TempDir()
	root := filepath.Join(data, "accounts")
	device := t.TempDir()
	marker := "fake-device-credential-marker"
	deviceFile := filepath.Join(device, "settings.json")
	if err := os.WriteFile(deviceFile, []byte(marker), 0600); err != nil {
		t.Fatal(err)
	}
	bins := t.TempDir()
	for binary, version := range map[string]string{"codex": "codex-cli 0.165.0", "claude": "2.1.0 (Claude Code)", "tmux": "tmux 3.5"} {
		if err := os.WriteFile(filepath.Join(bins, binary), []byte("#!/bin/sh\nprintf '%s\\n' '"+version+"'\n"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bins+string(os.PathListSeparator)+os.Getenv("PATH"))
	codex := &bindingProvider{AgentAccountProviderCLI: &agentaccount.Codex{DeviceHome: device}}
	claude := &bindingProvider{AgentAccountProviderCLI: &agentaccount.Claude{DeviceHome: device, DeviceConfig: deviceFile}}
	providers := []ports.AgentAccountProviderCLI{codex, claude}
	now := time.Now().UTC()
	homes := map[string]string{}
	for id, provider := range map[string]ports.AgentAccountProviderCLI{"A": codex, "B": claude, "C": codex, "D": codex, "E": claude} {
		home := filepath.Join(root, string(provider.Provider()), id)
		if err := provider.Prepare(home); err != nil {
			t.Fatal(err)
		}
		homes[id] = home
		if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte("fake-private-"+id), 0600); err != nil {
			t.Fatal(err)
		}
		if err := st.CreateAgentAccount(ctx, domain.AgentAccount{ID: id, Provider: provider.Provider(), Home: home, Email: id + "@example.invalid", ProfileKind: domain.AgentAccountProfileContained, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	for p, id := range map[domain.AgentAccountProvider]string{domain.AgentAccountProviderCodex: "A", domain.AgentAccountProviderClaude: "B"} {
		if err := st.SetAgentAccountDefault(ctx, p, id); err != nil {
			t.Fatal(err)
		}
	}
	project := domain.ProjectRecord{ID: "fake", Kind: domain.ProjectKindScratch, Path: t.TempDir(), RegisteredAt: now, Config: domain.ProjectConfig{Worker: domain.RoleOverride{Harness: domain.HarnessCodex, AgentAccountID: "A"}, Orchestrator: domain.RoleOverride{Harness: domain.HarnessClaudeCode, AgentAccountID: "B", AgentConfig: domain.AgentConfig{Model: "native"}}, Reviewers: []domain.ReviewerConfig{{Harness: domain.ReviewerCodex, AgentAccountID: "C", AgentConfig: domain.AgentConfig{Model: "native"}}}, Env: map[string]string{"CODEX_HOME": device, "CLAUDE_CONFIG_DIR": device, "OPENAI_API_KEY": marker, "ANTHROPIC_AUTH_TOKEN": marker}}}
	if err := st.UpsertProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	accounts := agentaccounts.New(st, providers, root, nil, nil)
	models := &bindingModels{}
	catalog := agentsvc.NewWithDeps(agentsvc.Deps{Cache: st, Discoverer: models, Context: ctx, ModelDiscoveryDir: filepath.Join(data, "discovery")})
	catalog.SetAccountExecution(accounts, st)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	agents, err := buildAgentResolver("codex", log)
	if err != nil {
		t.Fatal(err)
	}
	reviewers, err := reviewer.NewResolver()
	if err != nil {
		t.Fatal(err)
	}
	runtime := &bindingRuntime{}
	cfg := config.Config{DataDir: data, StateDir: data, Agent: "codex"}
	svc, reviews, lc, err := startSession(ctx, cfg, runtime, st, lifecycle.New(st, nil), nil, newSessionMessenger(st, runtime, log), telemetryadapter.NoopSink{}, nil, agents, catalog, nil, nil, nil, nil, nil, nil, nil, nil, nil, reviewers, nil, accounts, log)
	if err != nil {
		t.Fatal(err)
	}
	reviewsvc.WithPRRefresher(nil)(reviews.(*reviewsvc.Service))
	manager := lc.(*sessionmanager.Manager)
	manager.SetAgentAccounts(accounts)
	manager.SetModelCatalog(catalog)
	svc.SetAccountResolver(accounts)
	type result struct {
		session domain.Session
		err     error
	}
	workerCh, orchCh := make(chan result, 1), make(chan result, 1)
	request := ports.SpawnConfig{ProjectID: domain.ProjectID(project.ID), Kind: domain.KindWorker, Harness: domain.HarnessCodex, RequestedMode: domain.SessionModeTUI, Prompt: "fake task", ClientRequestID: "fixed-request", ClientRequestHash: "fixed-hash", AgentConfig: ports.AgentConfig{Model: "native", Effort: "high"}}
	go func() { s, _, _, e := svc.Spawn(ctx, request); workerCh <- result{s, e} }()
	go func() {
		s, e := svc.SpawnOrchestrator(ctx, domain.ProjectID(project.ID), false, domain.SessionModeTUI, "")
		orchCh <- result{s, e}
	}()
	worker, orch := <-workerCh, <-orchCh
	if worker.err != nil || orch.err != nil {
		t.Fatalf("A/B launch: %v %v", worker.err, orch.err)
	}
	if err := st.WriteSCMObservation(ctx, domain.PullRequest{URL: "https://github.com/fake/repo/pull/1", SessionID: worker.session.ID, Number: 1, HeadSHA: "fake-head", UpdatedAt: now}, nil, nil, nil, nil, ports.ReviewWritePreserve); err != nil {
		t.Fatal(err)
	}
	outcome, err := reviews.TriggerRequested(ctx, worker.session.ID, reviewsvc.TriggerRequest{Harness: domain.ReviewerCodex, InterfaceMode: domain.ReviewerInterfaceTUI})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for len(runtime.snapshot()) < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(runtime.snapshot()) != 3 {
		ids := []string{}
		for _, launch := range runtime.snapshot() {
			ids = append(ids, fmt.Sprint(launch.SessionID, ":review=", launch.Env["AO_REVIEW_SESSION_ID"]))
		}
		rec, _, _ := st.GetSession(ctx, orch.session.ID)
		t.Fatalf("expected A/B/C runtimes, got %v; review created=%t; orchestrator=%+v", ids, outcome.Created, rec)
	}
	for _, launch := range runtime.snapshot() {
		env := launch.Env
		var want string
		if env["AO_REVIEW_SESSION_ID"] != "" {
			want = "C"
		} else if launch.SessionID == worker.session.ID {
			want = "A"
		} else {
			want = "B"
		}
		key := "CODEX_HOME"
		if want == "B" {
			key = "CLAUDE_CONFIG_DIR"
		}
		if env[key] != homes[want] || env["OPENAI_API_KEY"] != "" || env["ANTHROPIC_AUTH_TOKEN"] != "" {
			t.Fatalf("launch escaped account %s", want)
		}
	}
	if err := st.SetAgentAccountDefault(ctx, domain.AgentAccountProviderCodex, "D"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetAgentAccountDefault(ctx, domain.AgentAccountProviderClaude, "E"); err != nil {
		t.Fatal(err)
	}
	project.Config.Worker.AgentAccountID = "D"
	project.Config.Orchestrator.AgentAccountID = "E"
	project.Config.Reviewers[0].AgentAccountID = "D"
	if err := st.UpsertProject(ctx, project); err != nil {
		t.Fatal(err)
	}

	replay, _, _, err := svc.Spawn(ctx, request)
	if err != nil || replay.ID != worker.session.ID {
		t.Fatalf("request replay drifted: %+v %v", replay, err)
	}
	changedRequest := request
	changedRequest.AgentAccountID = "D"
	changedRequest.ClientRequestHash = "different-account-hash"
	if _, _, _, e := svc.Spawn(ctx, changedRequest); e == nil {
		t.Fatal("request ID accepted a different explicit account")
	}

	restored := agentaccounts.New(st, providers, root, nil, nil)
	for id, want := range map[domain.SessionID]string{worker.session.ID: "A", orch.session.ID: "B"} {
		h := domain.HarnessCodex
		if want == "B" {
			h = domain.HarnessClaudeCode
		}
		env, e := restored.SessionLaunchEnv(ctx, id, h)
		key := "CODEX_HOME"
		if want == "B" {
			key = "CLAUDE_CONFIG_DIR"
		}
		if e != nil || env[key] != homes[want] {
			t.Fatalf("restore drifted: %s %v", want, e)
		}
	}
	rows, err := st.ListReviewsBySession(ctx, worker.session.ID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("review row: %v %v", rows, err)
	}
	bound, found, err := st.GetReviewerAgentAccount(ctx, rows[0].ID, domain.AgentAccountProviderCodex)
	if err != nil || !found || bound.AccountID != "C" || rows[0].ID == string(worker.session.ID) {
		t.Fatalf("reviewer borrowed worker: %+v %v", bound, err)
	}
	env, err := restored.ReviewerLaunchEnv(ctx, rows[0].ID, domain.HarnessCodex)
	if err != nil || env["CODEX_HOME"] != homes["C"] {
		t.Fatalf("reviewer restore drifted: %v", err)
	}
	_ = outcome
	// Simulate a daemon restart after confirmed runtime loss. Persisted bindings,
	// not the new project/global defaults, select both restored controllers.
	runtime.mu.Lock()
	for id := range runtime.alive {
		runtime.alive[id] = false
	}
	runtime.mu.Unlock()
	for _, id := range []domain.SessionID{worker.session.ID, orch.session.ID} {
		rec, found, e := st.GetSession(ctx, id)
		if e != nil || !found {
			t.Fatal(e)
		}
		rec.IsTerminated = true
		if e := st.UpdateSession(ctx, rec); e != nil {
			t.Fatal(e)
		}
	}
	_, _, newLC, e := startSession(ctx, cfg, runtime, st, lifecycle.New(st, nil), nil, newSessionMessenger(st, runtime, log), telemetryadapter.NoopSink{}, nil, agents, catalog, nil, nil, nil, nil, nil, nil, nil, nil, nil, reviewers, nil, restored, log)
	if e != nil {
		t.Fatal(e)
	}
	newManager := newLC.(*sessionmanager.Manager)
	newManager.SetAgentAccounts(restored)
	newManager.SetModelCatalog(catalog)
	before := len(runtime.snapshot())
	for _, id := range []domain.SessionID{worker.session.ID, orch.session.ID} {
		if _, e := newManager.RestoreWithMode(ctx, id); e != nil {
			t.Fatalf("manager restore %s: %v", id, e)
		}
	}
	seenRestored := map[string]bool{}
	for _, launch := range runtime.snapshot()[before:] {
		if launch.Env["AO_REVIEW_SESSION_ID"] != "" {
			if launch.Env["CODEX_HOME"] != homes["C"] {
				t.Fatal("restored reviewer drifted")
			}
			continue
		}
		want, key := "A", "CODEX_HOME"
		if launch.SessionID == orch.session.ID {
			want, key = "B", "CLAUDE_CONFIG_DIR"
		}
		if launch.Env[key] != homes[want] {
			t.Fatalf("manager restore selected new defaults for %s", want)
		}
		seenRestored[want] = true
	}
	if !seenRestored["A"] || !seenRestored["B"] {
		t.Fatalf("restored controllers missing: %v", seenRestored)
	}

	models.mu.Lock()
	seen := map[string]bool{}
	for _, r := range models.requests {
		home := r.Env["CODEX_HOME"]
		if r.AgentID == "claude-code" {
			home = r.Env["CLAUDE_CONFIG_DIR"]
		}
		if home == device || home == "" || strings.Contains(fmt.Sprint(r.Env), marker) {
			t.Fatal("model probe escaped selected account")
		}
		seen[filepath.Base(home)] = true
	}
	if !seen["A"] || !seen["B"] || !seen["C"] {
		t.Fatalf("account-scoped model lookups missing: %v", seen)
	}
	models.mu.Unlock()
	for _, p := range []*bindingProvider{codex, claude} {
		p.mu.Lock()
		for _, home := range p.homes {
			if home == "" || home == device {
				t.Error("identity probe used device")
			}
		}
		p.mu.Unlock()
	}
	bindings, e := restored.SessionAccountBindings(ctx, worker.session.ID)
	if e != nil || bindings[domain.AgentAccountProviderCodex].AccountID != "A" || bindings[domain.AgentAccountProviderCodex].Source != domain.AgentAccountSourceRole {
		t.Fatalf("API binding metadata drifted: %v %v", bindings, e)
	}
	bindingJSON, e := json.Marshal(bindings)
	if e != nil || strings.Contains(string(bindingJSON), root) {
		t.Fatal("binding projection exposed native home")
	}
	events, e := st.EventsAfter(ctx, 0, 10000)
	if e != nil {
		t.Fatal(e)
	}
	eventJSON, _ := json.Marshal(events)
	cacheRows, e := st.ListAgentModelCatalogs(ctx)
	if e != nil {
		t.Fatal(e)
	}
	cacheJSON, _ := json.Marshal(cacheRows)
	if strings.Contains(string(eventJSON), "fake-private-") || strings.Contains(string(cacheJSON), "fake-private-") {
		t.Fatal("private credential marker entered CDC/model DB")
	}

	got, err := os.ReadFile(deviceFile)
	info, statErr := os.Stat(deviceFile)
	if err != nil || statErr != nil || string(got) != marker || info.Mode().Perm() != 0600 {
		t.Fatal("fake external device changed")
	}
}
