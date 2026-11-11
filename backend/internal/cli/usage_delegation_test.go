package cli

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	hcgsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/hcg"
	projectsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/project"
	usagesvc "github.com/aoagents/agent-orchestrator/backend/internal/service/usage"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/components/hcg/reporting"
)

type delegationFixtureProvider func(context.Context, reporting.DelegationProviderRequest) (reporting.DelegationProviderResponse, error)

func (f delegationFixtureProvider) Call(ctx context.Context, r reporting.DelegationProviderRequest) (reporting.DelegationProviderResponse, error) {
	return f(ctx, r)
}
func fixtureDigest(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }

// Approve the explicitly supplied test installation, including RECORD and its
// path-dependent script hashes. No production verification bytes are excluded.
func fixtureRuntimeDigest(t *testing.T, venv string) string {
	t.Helper()
	root := filepath.Join(venv, "lib", "python3.12", "site-packages")
	aggregate := sha256.New()
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("nonregular fixture runtime file: %s", path)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		hash := sha256.New()
		_, copyErr := io.Copy(hash, file)
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		aggregate.Write([]byte(filepath.ToSlash(rel)))
		aggregate.Write([]byte{0})
		aggregate.Write(hash.Sum(nil))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(aggregate.Sum(nil))
}

func fixedDelegationRegistration(t *testing.T, family, project, body, root string, bindings []reporting.DelegationLabSession) reporting.DelegationRegistration {
	t.Helper()
	venv := filepath.Dir(filepath.Dir(os.Getenv("HCG_TEST_PAL_PYTHON")))
	read := func(path string) []byte {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	now := time.Now().UTC()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	grant := &reporting.DelegationLabGrant{RunID: "synthetic-run", OwnerRoot: root, CanonicalOwner: bindings[0], StartsAt: start, ExpiresAt: start.Add(24*time.Hour - time.Nanosecond), Profiles: map[string]reporting.DelegationProfileRef{"openai": {ID: "openai-fixture", Revision: "v1"}, "anthropic": {ID: "anthropic-fixture", Revision: "v1"}}, Bindings: bindings, ProviderCapNanos: map[string]int64{"openai": 10000000000, "anthropic": 10000000000}, TotalCapNanos: 20000000000, AttemptsPerProvider: 26}
	return reporting.DelegationRegistration{
		Grant: grant,
		Deployment: reporting.DelegationDeployment{
			Profile:       reporting.DelegationProfile{ID: family + "-fixture", Revision: "v1", BillingProvider: family, Model: "fixture-model", ContextTokens: 100000, MaxOutputTokens: 16384, OutputTokens: 128, Prices: reporting.DelegationPrices{Version: "synthetic-price-v1", Period: time.Now().UTC().Format("2006-01"), InputPerMillion: 50000000000, CachedInputPerMillion: 50000000000, LimitsCertified: true, BillableUsageCovered: true}},
			CredentialRef: reporting.DelegationCredentialReference{Provider: family, Name: "synthetic-ref", Version: "v1"}, Workspace: "synthetic-workspace",
			Installation: reporting.DelegationInstallation{Source: os.Getenv("HCG_TEST_PAL_SOURCE"), Venv: venv, Commit: "7afc7c1cc96e23992c8f105f960132c657883bb1", SourceDigest: "bac632c7fbe275c2f9389be3a56e69ada6a1d35d29cbc80ce575a46389608f4d", RuntimeDigest: fixtureRuntimeDigest(t, venv), PythonDigest: fixtureDigest(read(filepath.Join(venv, "bin", "python"))), ConfigDigest: fixtureDigest(read(filepath.Join(venv, "pyvenv.cfg"))), LockDigest: "799a72fd49b931a9f50152a6342f574efa1eb8eb89ba500e3f3e7f5859357666", PythonVersion: "3.12.3", AdapterVersion: "hcg-pal/1", TemplateVersion: "hcg.delegate-read/1"},
			Inputs:       []reporting.DelegationInputManifest{{ProjectID: project, Name: "synthetic-corpus", Revision: "r1", Files: []reporting.DelegationApprovedFile{{Path: "prompt.txt", SHA256: fixtureDigest([]byte(body))}}}},
		}, ExternalSpend: map[string]reporting.DelegationExternalSpend{"openai": {Known: true, OutsideGateOnly: true}, "anthropic": {Known: true, OutsideGateOnly: true}},
	}
}
func fixtureDelegationResponse(body string) reporting.DelegationProviderResponse {
	proposal, _ := json.Marshal(map[string]any{"schema": "hcg.delegate-read/1", "status": "ok", "findings": []any{map[string]any{"id": "f1", "kind": "fact", "text": "fixture-answer", "evidence_ids": []string{"e1"}}}, "evidence": []any{map[string]any{"id": "e1", "source_id": "s0000", "revision": "r1", "content_sha256": fixtureDigest([]byte(body)), "line_start": 1, "line_end": 1, "quote": strings.Split(body, "\n")[0]}}})
	raw, _ := json.Marshal(map[string]any{"id": "fixture-response", "model": "fixture-model", "choices": []any{map[string]any{"message": map[string]any{"content": string(proposal)}, "finish_reason": "stop"}}, "usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 2, "total_tokens": 12}})
	return reporting.DelegationProviderResponse{Status: 200, RequestID: "same-request", Body: raw}
}

// The real CLI/HTTP/service/PAL/receipt/SQLite caller uses synthetic files and
// a constructor-injected provider. No key, remote endpoint or agent is used.
func TestHCGDelegationSameCallerOffline(t *testing.T) {
	if runtime.GOOS != "linux" || os.Getenv("HCG_PAL_TESTS") != "1" {
		t.Skip("isolated fixed PAL required")
	}
	for _, family := range []string{"openai", "anthropic"} {
		t.Run(family, func(t *testing.T) {
			ctx := t.Context()
			cfg := setConfigEnv(t)
			store, err := sqlite.Open(cfg.dataDir)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			if err := os.Chmod(cfg.dataDir, 0700); err != nil {
				t.Fatal(err)
			}
			worktree := t.TempDir()
			body := "SYNTHETIC_SOURCE_142\nDOCUMENT_DIRECTIVE_IGNORE_THE_QUESTION\n"
			if err := os.WriteFile(filepath.Join(worktree, "prompt.txt"), []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			enabled := true
			ref := domain.HCGProfileRef{ID: family + "-fixture", Revision: "v1"}
			project := domain.ProjectRecord{ID: "synthetic-project", Path: worktree, RegisteredAt: time.Now(), Config: domain.ProjectConfig{HCGDelegation: domain.HCGDelegationConfig{Enabled: &enabled, AllowedProfiles: []domain.HCGProfileRef{ref}, SelectedProfile: &ref}}}
			if err := store.UpsertProject(ctx, project); err != nil {
				t.Fatal(err)
			}
			rec, err := store.CreateSession(ctx, domain.SessionRecord{ProjectID: domain.ProjectID(project.ID), Kind: domain.KindWorker, Harness: domain.HarnessCodex, Mode: domain.SessionModeChat, CreatedAt: time.Now(), UpdatedAt: time.Now(), Metadata: domain.SessionMetadata{WorkspacePath: worktree, ProviderConversationID: "synthetic-native", AgentSettingsLaunchID: "synthetic-generation", ControllerGeneration: "synthetic-generation"}})
			if err != nil {
				t.Fatal(err)
			}
			// Match Chat's real durable success path; CreateSession returns the
			// input record but does not persist the dedicated launch identity.
			if err := store.ClaimChatControllerGeneration(ctx, rec.ID, rec.Metadata.ControllerGeneration); err != nil {
				t.Fatal(err)
			}
			rec, _, err = store.GetSession(ctx, rec.ID)
			if err != nil {
				t.Fatal(err)
			}
			if rec.Metadata.AgentSettingsLaunchID != rec.Metadata.ControllerGeneration || rec.Metadata.WorkspacePath != worktree {
				t.Fatal("fresh durable owner not published")
			}
			var calls, mode atomic.Int32
			started := make(chan struct{})
			provider := delegationFixtureProvider(func(ctx context.Context, r reporting.DelegationProviderRequest) (reporting.DelegationProviderResponse, error) {
				calls.Add(1)
				if r.Endpoint != "https://api."+family+".com/v1/chat/completions" || r.BillingProvider != family || r.CredentialRef.Provider != family || r.CredentialRef.Version != "v1" || !strings.Contains(string(r.Payload), "QUESTION_MUST_SURVIVE") {
					t.Errorf("incorrect fixed provider request: %+v", r)
				}
				if mode.Load() == 2 {
					close(started)
					<-ctx.Done()
					return reporting.DelegationProviderResponse{}, ctx.Err()
				}
				response := fixtureDelegationResponse(body)
				if family == "openai" {
					var payload map[string]any
					if json.Unmarshal(r.Payload, &payload) != nil || payload["model"] != "gpt-6-luna" || payload["reasoning_effort"] != "medium" || payload["service_tier"] != "default" || payload["max_completion_tokens"] != float64(2048) {
						t.Error("Luna payload lost immutable mode")
					}
					var envelope map[string]any
					_ = json.Unmarshal(response.Body, &envelope)
					envelope["model"] = "gpt-6-luna"
					envelope["service_tier"] = "default"
					envelope["usage"].(map[string]any)["prompt_tokens_details"] = map[string]int64{"cached_tokens": 2, "cache_write_tokens": 3}
					response.Body, _ = json.Marshal(envelope)
				}
				if mode.Load() == 1 {
					response.Body = []byte(`{"id":"fixture-response","model":"fixture-model","choices":[{"message":{"content":"not-json"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`)
				}
				return response, nil
			})
			svc := hcgsvc.NewWithDelegationProvider(ctx, cfg.dataDir, store, provider, nil)
			defer svc.Close()
			router := httpd.NewRouterWithControl(config.Config{DataDir: cfg.dataDir}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, httpd.APIDeps{HCGDelegation: svc}, httpd.ControlDeps{})
			server := httptest.NewServer(router)
			defer server.Close()
			writeRunFileFor(t, cfg, server)
			call := func(exit int, args ...string) string {
				t.Helper()
				out, stderr, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, append([]string{"usage", "delegation"}, args...)...)
				if ExitCode(err) != exit {
					t.Fatalf("%v exit=%d err=%v stderr=%s out=%s", args, ExitCode(err), err, stderr, out)
				}
				return out
			}
			approved := fixedDelegationRegistration(t, family, project.ID, body, cfg.dataDir, []reporting.DelegationLabSession{{ProjectID: project.ID, SessionID: string(rec.ID)}})
			if family == "openai" {
				p := &approved.Deployment.Profile
				p.Model = "gpt-6-luna"
				p.ContextTokens = 1050000
				p.MaxOutputTokens = 128000
				p.OutputTokens = 2048
				p.ReasoningEffort = "medium"
				p.ServiceTier = "default"
				w := int64(125000000)
				p.Prices.InputPerMillion = 100000000
				p.Prices.CachedInputPerMillion = 10000000
				p.Prices.OutputPerMillion = 500000000
				p.Prices.CacheWritePerMillion = &w
				p.Prices.LongContext = &reporting.DelegationLongContextPrices{ThresholdInputTokens: 272000, InputPerMillion: 200000000, CachedInputPerMillion: 20000000, CacheWritePerMillion: 250000000, OutputPerMillion: 750000000}
			}

			wrongFamily := approved
			wrongFamily.Deployment.CredentialRef.Provider = map[string]string{"openai": "anthropic", "anthropic": "openai"}[family]
			rejected, _ := json.Marshal(wrongFamily)
			call(1, "register", "--profile-json", string(rejected))
			unknownCaps := approved
			unknownCaps.Deployment.Profile.Prices.LimitsCertified = false
			rejected, _ = json.Marshal(unknownCaps)
			call(1, "register", "--profile-json", string(rejected))
			registration, _ := json.Marshal(approved)
			var extra map[string]any
			if err := json.Unmarshal(registration, &extra); err != nil {
				t.Fatal(err)
			}
			extra["deployment"].(map[string]any)["endpoint"] = "https://synthetic.invalid/"
			rejected, _ = json.Marshal(extra)
			call(1, "register", "--profile-json", string(rejected))
			if calls.Load() != 0 {
				t.Fatal("rejected registry input dispatched")
			}
			call(0, "register", "--profile-json", string(registration))
			svc.ReportingLaunchReady(ports.HCGReportingLaunch{DelegationConfigured: true, Origin: ports.HCGReportingMainChat, Owner: domain.SessionConversationOwner(rec.ID), SessionID: rec.ID, ProjectID: rec.ProjectID, Kind: rec.Kind, Harness: rec.Harness, Mode: rec.Mode, DataRoot: cfg.dataDir, WorkspacePath: worktree, CommittedOwner: rec.ControllerOwner(), Fresh: true})
			deadline := time.Now().Add(3 * time.Second)
			for {
				view, err := svc.GetDelegation(ctx, rec.ID)
				if err != nil {
					t.Fatal(err)
				}
				if view.Active {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("launch not active: %+v", view)
				}
				time.Sleep(time.Millisecond)
			}
			before, _ := json.Marshal(rec)
			var reply reporting.DelegationReply
			if err := json.Unmarshal([]byte(call(0, "read", "--session", string(rec.ID), "--question", "QUESTION_MUST_SURVIVE", "--path", "prompt.txt")), &reply); err != nil {
				t.Fatal(err)
			}
			if reply.Status != "ok" || reply.Coverage.Status != "complete" || len(reply.Evidence) != 1 || reply.Evidence[0].Verification != "machine_checked" || calls.Load() != 1 {
				t.Fatalf("reply=%+v calls=%d", reply, calls.Load())
			}
			ledger, err := usagesvc.NewEventsReader(store).Events(ctx, rec.ID, 0, nil, 100)
			if err != nil {
				t.Fatal(err)
			}
			if len(ledger.Events) != 1 || len(ledger.Bindings) != 0 {
				t.Fatalf("canonical ledger=%+v", ledger)
			}
			event := ledger.Events[0]
			if event.Execution == nil || event.Execution.ProjectID != project.ID || event.Execution.ProfileID != ref.ID || event.Execution.RequestID != "same-request" || event.InputTokens == nil || *event.InputTokens != 10 || (family != "openai" && event.CachedInputTokens != nil) || event.OutputTokens == nil || *event.OutputTokens != 2 {
				t.Fatalf("receipt attribution=%+v", event)
			}
			if family == "openai" {
				registryBytes, err := os.ReadFile(filepath.Join(cfg.dataDir, "hcg", "delegation", "registry.json"))
				if err != nil {
					t.Fatal(err)
				}
				var registry struct {
					History map[string]struct {
						Profile json.RawMessage `json:"profile"`
					}
				}
				if err := json.Unmarshal(registryBytes, &registry); err != nil {
					t.Fatal(err)
				}
				snapshot, ok := registry.History[event.Execution.ProfileDigest]
				if !ok || fixtureDigest(snapshot.Profile) != event.Execution.ProfileDigest {
					t.Fatal("canonical profile digest differs from dispatched snapshot")
				}
				var registered reporting.DelegationProfile
				if err := json.Unmarshal(snapshot.Profile, &registered); err != nil {
					t.Fatal(err)
				}
				wantProfile, _ := json.Marshal(approved.Deployment.Profile)
				gotProfile, _ := json.Marshal(registered)
				if string(wantProfile) != string(gotProfile) {
					t.Fatal("HTTP registration dropped profile fields")
				}
				usageProjection, _ := json.Marshal(event.ProviderUsage)
				db, err := sql.Open("sqlite", filepath.Join(cfg.dataDir, "ao.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				var inputCost, cachedCost, outputCost int64
				if err := db.QueryRowContext(ctx, "SELECT input_cost_nanos,cached_input_cost_nanos,output_cost_nanos FROM model_usage_events WHERE execution_source_id = ?", event.Execution.ID).Scan(&inputCost, &cachedCost, &outputCost); err != nil {
					t.Fatal(err)
				}
				if inputCost != 875 || cachedCost != 20 || outputCost != 1000 {
					t.Fatalf("canonical costs %d/%d/%d", inputCost, cachedCost, outputCost)
				}
				if event.Execution.RequestedServiceTier != "default" || event.Execution.ObservedServiceTier != "default" || event.Execution.ServiceTierStatus != "verified_default" || event.CachedInputTokens == nil || *event.CachedInputTokens != 2 || event.UncachedInputTokens == nil || *event.UncachedInputTokens != 8 || !strings.Contains(string(usageProjection), `"cache_write_input_tokens":3`) {
					t.Fatalf("Luna canonical evidence=%+v", event)
				}
				// This representative Luna chain dispatches exactly once. Other negative
				// cases live at the parser/payload/canonical boundaries.
				return
			}

			current, _, err := store.GetSession(ctx, rec.ID)
			if err != nil {
				t.Fatal(err)
			}
			after, _ := json.Marshal(current)
			if string(before) != string(after) {
				t.Fatal("delegation rewrote native session")
			}
			currentProject, _, err := store.GetProject(ctx, project.ID)
			if err != nil {
				t.Fatal(err)
			}
			want, _ := json.Marshal(project.Config)
			got, _ := json.Marshal(currentProject.Config)
			if string(want) != string(got) {
				t.Fatal("delegation rewrote project policy")
			}
			mode.Store(1)
			failed := call(1, "read", "--session", string(rec.ID), "--question", "QUESTION_MUST_SURVIVE", "--path", "prompt.txt")
			var invalid reporting.DelegationReply
			if json.Unmarshal([]byte(failed), &invalid) != nil || invalid.Status != "blocked" || invalid.Answer != "" || len(invalid.Evidence) != 0 {
				t.Fatalf("schema failure exposed partial answer: %s", failed)
			}
			mode.Store(2)
			type outcome struct {
				out string
				err error
			}
			finished := make(chan outcome, 1)
			go func() {
				out, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "usage", "delegation", "read", "--session", string(rec.ID), "--question", "QUESTION_MUST_SURVIVE", "--path", "prompt.txt")
				finished <- outcome{out, err}
			}()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("in-flight provider not started")
			}
			call(0, "emergency-stop", "--enabled", "true")
			select {
			case result := <-finished:
				var stopped reporting.DelegationReply
				if ExitCode(result.err) != 1 || json.Unmarshal([]byte(result.out), &stopped) != nil || stopped.Status != "unavailable" || stopped.Answer != "" || len(stopped.Evidence) != 0 {
					t.Fatalf("host-stop caller=%s %v", result.out, result.err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("host-stop did not release caller")
			}

			blocked := call(1, "read", "--session", string(rec.ID), "--question", "QUESTION_MUST_SURVIVE", "--path", "prompt.txt")
			if !strings.Contains(blocked, "emergency_stop") || calls.Load() != 3 {
				t.Fatal("stopped caller dispatched")
			}
		})
	}
}

func TestHCGDelegationSharedHostRevisionBoundary(t *testing.T) {
	if runtime.GOOS != "linux" || os.Getenv("HCG_PAL_TESTS") != "1" {
		t.Skip("isolated fixed PAL required")
	}
	ctx := t.Context()
	cfg := setConfigEnv(t)
	dbDir := cfg.dataDir
	store, err := sqlite.Open(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := os.Chmod(cfg.dataDir, 0700); err != nil {
		t.Fatal(err)
	}
	body := "SYNTHETIC_SOURCE_142\nDOCUMENT_DIRECTIVE_IGNORE_THE_QUESTION\n"
	projects := map[string]domain.ProjectRecord{}
	sessions := map[string]domain.SessionRecord{}
	for _, family := range []string{"openai", "anthropic"} {
		path := t.TempDir()
		if err := os.WriteFile(filepath.Join(path, "prompt.txt"), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		on := true
		ref := domain.HCGProfileRef{ID: family + "-fixture", Revision: "v1"}
		p := domain.ProjectRecord{ID: family + "-project", Path: path, Config: domain.ProjectConfig{HCGDelegation: domain.HCGDelegationConfig{Enabled: &on, AllowedProfiles: []domain.HCGProfileRef{ref}, SelectedProfile: &ref}}}
		if err := store.UpsertProject(ctx, p); err != nil {
			t.Fatal(err)
		}
		rec, err := store.CreateSession(ctx, domain.SessionRecord{ProjectID: domain.ProjectID(p.ID), Kind: domain.KindWorker, Harness: domain.HarnessCodex, Mode: domain.SessionModeChat, Metadata: domain.SessionMetadata{WorkspacePath: path, ControllerGeneration: "generation-" + family, ProviderConversationID: "native-" + family}})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.ClaimChatControllerGeneration(ctx, rec.ID, rec.Metadata.ControllerGeneration); err != nil {
			t.Fatal(err)
		}
		rec, _, err = store.GetSession(ctx, rec.ID)
		if err != nil {
			t.Fatal(err)
		}
		projects[family], sessions[family] = p, rec
	}
	var phase, calls atomic.Int32
	reserved := make(chan struct{}, 1)
	dispatched := make(chan reporting.DelegationProviderRequest, 1)
	release := make(chan struct{})
	defer close(release)
	provider := delegationFixtureProvider(func(ctx context.Context, r reporting.DelegationProviderRequest) (reporting.DelegationProviderResponse, error) {
		calls.Add(1)
		if phase.Load() == 2 {
			dispatched <- r
			<-release
		}
		return fixtureDelegationResponse(body), nil
	})
	checkpoint := func(ctx context.Context, point string) {
		if point == "reserved" && phase.Load() == 1 {
			reserved <- struct{}{}
			<-ctx.Done()
		}
	}
	pausedStore := &delegationPausedProjectStore{Store: store, entered: make(chan struct{}, 1), release: make(chan struct{}, 1)}
	svc := hcgsvc.NewWithDelegationProvider(ctx, cfg.dataDir, pausedStore, provider, checkpoint)
	defer svc.Close()
	projectService := projectsvc.NewWithDeps(projectsvc.Deps{Store: store, DelegationPolicyChanged: svc.DelegationProjectPolicyChanged})
	router := httpd.NewRouterWithControl(config.Config{DataDir: cfg.dataDir}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, httpd.APIDeps{HCGDelegation: svc}, httpd.ControlDeps{})
	server := httptest.NewServer(router)
	defer server.Close()
	writeRunFileFor(t, cfg, server)
	type outcome struct {
		out string
		err error
	}
	execute := func(args ...string) outcome {
		out, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, append([]string{"usage", "delegation"}, args...)...)
		return outcome{out, err}
	}
	call := func(exit int, args ...string) string {
		t.Helper()
		r := execute(args...)
		if ExitCode(r.err) != exit {
			t.Fatalf("%v exit=%d err=%v out=%s", args, ExitCode(r.err), r.err, r.out)
		}
		return r.out
	}
	registrations := map[string]reporting.DelegationRegistration{}
	register := func(r reporting.DelegationRegistration) {
		t.Helper()
		raw, _ := json.Marshal(r)
		call(0, "register", "--profile-json", string(raw))
	}
	readArgs := func(family string) []string {
		return []string{"read", "--session", string(sessions[family].ID), "--question", "QUESTION_MUST_SURVIVE", "--path", "prompt.txt"}
	}
	selectRevision := func(family, revision string) {
		t.Helper()
		p := projects[family]
		ref := domain.HCGProfileRef{ID: family + "-fixture", Revision: revision}
		p.Config.HCGDelegation.AllowedProfiles = []domain.HCGProfileRef{ref}
		p.Config.HCGDelegation.SelectedProfile = &ref
		if _, err := projectService.SetConfig(ctx, domain.ProjectID(p.ID), projectsvc.SetConfigInput{Config: p.Config}); err != nil {
			t.Fatal(err)
		}
		projects[family] = p
	}
	for _, family := range []string{"openai", "anthropic"} {
		registration := fixedDelegationRegistration(t, family, projects[family].ID, body, cfg.dataDir, []reporting.DelegationLabSession{{ProjectID: projects["openai"].ID, SessionID: string(sessions["openai"].ID)}, {ProjectID: projects["anthropic"].ID, SessionID: string(sessions["anthropic"].ID)}})
		registrations[family] = registration
		register(registration)
	}
	for _, family := range []string{"openai", "anthropic"} {
		rec := sessions[family]
		svc.ReportingLaunchReady(ports.HCGReportingLaunch{DelegationConfigured: true, Origin: ports.HCGReportingMainChat, Owner: domain.SessionConversationOwner(rec.ID), SessionID: rec.ID, ProjectID: rec.ProjectID, Kind: rec.Kind, Harness: rec.Harness, Mode: rec.Mode, DataRoot: cfg.dataDir, WorkspacePath: rec.Metadata.WorkspacePath, CommittedOwner: rec.ControllerOwner(), Fresh: true})
		until := time.Now().Add(3 * time.Second)
		for {
			view, err := svc.GetDelegation(ctx, rec.ID)
			if err != nil {
				t.Fatal(err)
			}
			if view.Active {
				break
			}
			if time.Now().After(until) {
				t.Fatalf("launch=%+v", view)
			}
			time.Sleep(time.Millisecond)
		}
		call(0, readArgs(family)...)
		ledger, err := usagesvc.NewEventsReader(store).Events(ctx, rec.ID, 0, nil, 100)
		if err != nil {
			t.Fatal(err)
		}
		if len(ledger.Events) != 1 || ledger.Events[0].Execution == nil || ledger.Events[0].Execution.ProjectID != projects[family].ID || ledger.Events[0].Execution.ProfileID != family+"-fixture" || ledger.Events[0].Execution.RequestID != "same-request" {
			t.Fatalf("cross-family receipt merged: %+v", ledger)
		}
	}
	if calls.Load() != 2 {
		t.Fatal("both fixed profiles not used on same host")
	}
	// Freeze old policy facts before admission; every normal writer must
	// invalidate the old request even when off immediately returns to on.
	for _, cause := range []string{"project", "role", "import", "host", "session"} {
		for _, aba := range []bool{false, true} {
			pausedStore.pause.Store(true)
			pending := make(chan outcome, 1)
			go func() { pending <- execute(readArgs("openai")...) }()
			select {
			case <-pausedStore.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("policy facts did not pause")
			}
			change := func(on bool) {
				t.Helper()
				switch cause {
				case "project", "role", "import":
					p := projects["openai"]
					if cause == "role" {
						p.Config.Worker.HCGDelegation.Enabled = &on
					} else {
						p.Config.HCGDelegation.Enabled = &on
					}
					if cause == "import" {
						if err := projectService.ImportTarget(store).ImportWorkspaceProject(ctx, p, nil); err != nil {
							t.Fatal(err)
						}
					} else if _, err := projectService.SetConfig(ctx, domain.ProjectID(p.ID), projectsvc.SetConfigInput{Config: p.Config}); err != nil {
						t.Fatal(err)
					}
				case "host":
					call(0, "emergency-stop", "--enabled", map[bool]string{true: "false", false: "true"}[on])
				case "session":
					call(0, "set", "--session", string(sessions["openai"].ID), "--enabled", map[bool]string{true: "true", false: "false"}[on])
				}
			}
			change(false)
			if aba {
				change(true)
			}
			pausedStore.release <- struct{}{}
			select {
			case result := <-pending:
				if ExitCode(result.err) != 1 || !strings.Contains(result.out, "policy_revision_changed") {
					t.Fatalf("%s ABA=%v admitted old policy: %s %v", cause, aba, result.out, result.err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("old policy caller did not exit")
			}
			if calls.Load() != 2 {
				t.Fatalf("%s ABA=%v dispatched old policy", cause, aba)
			}
			change(true)
			if cause == "session" {
				call(0, "clear", "--session", string(sessions["openai"].ID))
			}
		}
	}
	// An out-of-grant revision is refused; a legal host-stop ABA still cancels the reservation.
	phase.Store(1)
	finished := make(chan outcome, 1)
	go func() { finished <- execute(readArgs("openai")...) }()
	select {
	case <-reserved:
	case <-time.After(5 * time.Second):
		t.Fatal("reservation did not pause")
	}
	v2 := registrations["openai"]
	v2.Deployment.Profile.Revision = "v2"
	v2.Deployment.CredentialRef.Version = "v2"
	v2.Deployment.Profile.Prices.Version = "price-v2"
	rejectedRevision, _ := json.Marshal(v2)
	call(1, "register", "--profile-json", string(rejectedRevision))
	call(0, "emergency-stop", "--enabled=true")
	call(0, "emergency-stop", "--enabled=false")
	select {
	case result := <-finished:
		if ExitCode(result.err) != 1 {
			t.Fatalf("old reserved caller=%s", result.out)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("host update blocked behind reservation")
	}
	if calls.Load() != 2 {
		t.Fatal("stale reserved attempt dispatched")
	}
	phase.Store(0)
	selectRevision("openai", "v1")
	call(0, readArgs("openai")...)
	view, err := svc.GetDelegation(ctx, sessions["openai"].ID)
	if err != nil {
		t.Fatal(err)
	}
	if view.MountProfile == nil || view.MountProfile.Revision != "v1" || view.ResolvedProfile.Revision != "v1" {
		t.Fatalf("launch/current mixed: %+v", view)
	}
	// Once dispatched, the original granted v1 remains the receipt/price identity
	// through host-stop and a refused revision update, including a late reply.
	phase.Store(2)
	go func() { finished <- execute(readArgs("openai")...) }()
	var old reporting.DelegationProviderRequest
	select {
	case old = <-dispatched:
	case <-time.After(5 * time.Second):
		t.Fatal("dispatch did not pause")
	}
	v3 := v2
	v3.Deployment.Profile.Revision = "v3"
	v3.Deployment.Profile.Prices.Version = "price-v3"
	v3.Deployment.Profile.Prices.InputPerMillion = 300000000000
	v3.Deployment.Profile.Prices.CachedInputPerMillion = 300000000000
	v3.Deployment.CredentialRef.Version = "v3"
	rejectedRevision, _ = json.Marshal(v3)
	call(1, "register", "--profile-json", string(rejectedRevision))
	call(0, "emergency-stop", "--enabled=true")
	call(0, "emergency-stop", "--enabled=false")
	select {
	case result := <-finished:
		if ExitCode(result.err) != 1 || strings.Contains(result.out, "fixture-answer") {
			t.Fatalf("cancelled caller got stale success: %s", result.out)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("uncooperative provider blocked caller")
	}
	before := calls.Load()
	call(1, readArgs("anthropic")...)
	if calls.Load() != before {
		t.Fatal("in-flight hold allowed another project/provider")
	}
	if old.ProfileRevision != "v1" || old.CredentialRef.Version != "v1" {
		t.Fatalf("transport re-resolved current: %+v", old)
	}
	release <- struct{}{}
	until := time.Now().Add(5 * time.Second)
	for {
		through, err := store.GetUsageEventsThroughID(ctx, sessions["openai"].ID)
		if err != nil {
			t.Fatal(err)
		}
		ledger, err := store.ListUsageEventsPage(ctx, sessions["openai"].ID, 0, through, 100)
		if err != nil {
			t.Fatal(err)
		}
		if len(ledger) == 3 {
			last := ledger[2]
			if last.Execution == nil || last.Execution.ProfileRevision != "v1" {
				t.Fatalf("late receipt repriced: %+v", last)
			}
			// This product reader intentionally omits costs. Verify only this
			// fixture's canonical SQLite row via a separate read-only handle.
			uri := url.URL{Scheme: "file", Path: filepath.Join(dbDir, "ao.db")}
			query := uri.Query()
			query.Set("mode", "ro")
			uri.RawQuery = query.Encode()
			readonly, err := sql.Open("sqlite", uri.String())
			if err != nil {
				t.Fatal(err)
			}
			defer readonly.Close()
			var price string
			var input, output, total sql.NullInt64
			err = readonly.QueryRowContext(ctx, `SELECT e.pricing_version,e.input_cost_nanos,e.output_cost_nanos,e.estimated_cost_nanos
              FROM model_usage_events e JOIN usage_execution_sources x ON x.id=e.execution_source_id
              WHERE e.id=? AND x.session_id=? AND x.project_id=? AND x.profile_id=? AND x.profile_revision=? AND x.billing_provider=?`, last.ID, string(sessions["openai"].ID), projects["openai"].ID, "openai-fixture", "v1", "openai").Scan(&price, &input, &output, &total)
			if err != nil {
				t.Fatal(err)
			}
			if price != "synthetic-price-v1" || input.Valid || !output.Valid || output.Int64 != 0 || !total.Valid || total.Int64 != 500000 {
				t.Fatalf("late canonical price/cost changed: %q input=%v output=%v total=%v", price, input, output, total)
			}
			break
		}
		if time.Now().After(until) {
			t.Fatalf("late receipt not canonical: %+v", ledger)
		}
		time.Sleep(time.Millisecond)
	}
}

// Only the synthetic store's read is paused; durable writers use the same
// production project service and imported-config adapter as the daemon.
type delegationPausedProjectStore struct {
	*sqlite.Store
	pause            atomic.Bool
	entered, release chan struct{}
}

func (s *delegationPausedProjectStore) GetProject(ctx context.Context, id string) (domain.ProjectRecord, bool, error) {
	p, ok, err := s.Store.GetProject(ctx, id)
	if s.pause.CompareAndSwap(true, false) {
		s.entered <- struct{}{}
		select {
		case <-s.release:
		case <-ctx.Done():
			return p, false, ctx.Err()
		}
	}
	return p, ok, err
}
