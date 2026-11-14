package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/datadirlock"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	hcgsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/hcg"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/components/hcg/reporting"
)

type labCanonicalStore struct {
	*sqlite.Store
	refuse atomic.Bool
}

func (s *labCanonicalStore) InsertExecutionUsage(ctx context.Context, r domain.ExecutionUsageReceipt, key string) error {
	if s.refuse.Load() {
		return errors.New("synthetic canonical write failure")
	}
	return s.Store.InsertExecutionUsage(ctx, r, key)
}

type labCallerFixture struct {
	cfg                     testConfig
	family, body, workspace string
	session                 domain.SessionRecord
	registration            reporting.DelegationRegistration
	store                   *labCanonicalStore
	service                 *hcgsvc.Service
	server                  *httptest.Server
	lock                    *datadirlock.Lock
}

func newLabCaller(t *testing.T, family string, provider reporting.DelegationProvider) *labCallerFixture {
	t.Helper()
	if runtime.GOOS != "linux" || os.Getenv("HCG_PAL_TESTS") != "1" {
		t.Skip("fixed isolated PAL required")
	}
	f := &labCallerFixture{cfg: setConfigEnv(t), family: family, body: "SYNTHETIC_SOURCE_142\nDOCUMENT_DIRECTIVE_IGNORE_THE_QUESTION\n", workspace: t.TempDir()}
	if err := os.MkdirAll(f.cfg.dataDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.workspace, "prompt.txt"), []byte(f.body), 0600); err != nil {
		t.Fatal(err)
	}
	f.open(t)
	on := true
	ref := domain.HCGProfileRef{ID: family + "-fixture", Revision: "v1"}
	p := domain.ProjectRecord{ID: "synthetic-project", Path: f.workspace, RegisteredAt: time.Now(), Config: domain.ProjectConfig{HCGDelegation: domain.HCGDelegationConfig{Enabled: &on, AllowedProfiles: []domain.HCGProfileRef{ref}, SelectedProfile: &ref}}}
	if err := f.store.UpsertProject(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	rec, err := f.store.CreateSession(t.Context(), domain.SessionRecord{ProjectID: domain.ProjectID(p.ID), Kind: domain.KindWorker, Harness: domain.HarnessCodex, Mode: domain.SessionModeChat, CreatedAt: time.Now(), UpdatedAt: time.Now(), Metadata: domain.SessionMetadata{WorkspacePath: f.workspace, ControllerGeneration: "synthetic-generation", ProviderConversationID: "synthetic-native"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.ClaimChatControllerGeneration(t.Context(), rec.ID, rec.Metadata.ControllerGeneration); err != nil {
		t.Fatal(err)
	}
	f.session, _, err = f.store.GetSession(t.Context(), rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.registration = fixedDelegationRegistration(t, family, p.ID, f.body, f.cfg.dataDir, []reporting.DelegationLabSession{{ProjectID: p.ID, SessionID: string(rec.ID)}})
	f.start(t, provider)
	t.Cleanup(f.close)
	return f
}

func (f *labCallerFixture) open(t *testing.T) {
	t.Helper()
	var err error
	f.lock, err = datadirlock.Acquire(t.Context(), f.cfg.dataDir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := hcgsvc.CheckDelegationOwnerStartup(t.Context(), f.cfg.dataDir); err != nil {
		t.Fatal(err)
	}
	store, err := sqlite.Open(f.cfg.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	f.store = &labCanonicalStore{Store: store}
}
func (f *labCallerFixture) start(t *testing.T, p reporting.DelegationProvider) {
	t.Helper()
	f.service = hcgsvc.NewWithDelegationProvider(t.Context(), f.cfg.dataDir, f.store, p, nil)
	router := httpd.NewRouterWithControl(config.Config{DataDir: f.cfg.dataDir}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, httpd.APIDeps{HCGDelegation: f.service}, httpd.ControlDeps{})
	f.server = newFixtureServer(t, router)
	writeRunFileFor(t, f.cfg, f.server)
}
func (f *labCallerFixture) close() {
	if f.service != nil {
		f.service.Close()
		f.service = nil
	}
	if f.server != nil {
		f.server.Close()
		f.server = nil
	}
	if f.store != nil {
		f.store.Close()
		f.store = nil
	}
	if f.lock != nil {
		f.lock.Release()
		f.lock = nil
	}
}
func (f *labCallerFixture) call(t *testing.T, exit int, args ...string) string {
	t.Helper()
	out, stderr, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, append([]string{"usage", "delegation"}, args...)...)
	if ExitCode(err) != exit {
		t.Fatalf("lab command exit=%d want=%d err=%v stderr=%s", ExitCode(err), exit, err, stderr)
	}
	return out
}
func (f *labCallerFixture) registerAndLaunch(t *testing.T) {
	t.Helper()
	raw, err := json.Marshal(f.registration)
	if err != nil {
		t.Fatal(err)
	}
	f.call(t, 0, "register", "--profile-json", string(raw))
	r := f.session
	f.service.ReportingLaunchReady(ports.HCGReportingLaunch{DelegationConfigured: true, Origin: ports.HCGReportingMainChat, Owner: domain.SessionConversationOwner(r.ID), SessionID: r.ID, ProjectID: r.ProjectID, Kind: r.Kind, Harness: r.Harness, Mode: r.Mode, DataRoot: f.cfg.dataDir, WorkspacePath: f.workspace, CommittedOwner: r.ControllerOwner(), Fresh: true})
	until := time.Now().Add(3 * time.Second)
	for {
		v, err := f.service.GetDelegation(t.Context(), r.ID)
		if err != nil {
			t.Fatal(err)
		}
		if v.Active {
			return
		}
		if time.Now().After(until) {
			t.Fatalf("lab launch unavailable: %s", v.Reason)
		}
		time.Sleep(time.Millisecond)
	}
}
func (f *labCallerFixture) readArgs() []string {
	return []string{"read", "--session", string(f.session.ID), "--question", "QUESTION_MUST_SURVIVE", "--path", "prompt.txt", "--json"}
}

func TestHCGLabCanonicalOwnerRecoveryDoesNotResend(t *testing.T) {
	for _, family := range []string{"openai", "anthropic"} {
		t.Run(family, func(t *testing.T) {
			var calls atomic.Int32
			provider := delegationFixtureProvider(func(context.Context, reporting.DelegationProviderRequest) (reporting.DelegationProviderResponse, error) {
				calls.Add(1)
				return fixtureDelegationResponse("SYNTHETIC_SOURCE_142\nDOCUMENT_DIRECTIVE_IGNORE_THE_QUESTION\n"), nil
			})
			f := newLabCaller(t, family, provider)
			f.registerAndLaunch(t)
			f.store.refuse.Store(true)
			out := f.call(t, 1, f.readArgs()...)
			if !strings.Contains(out, "usage_pending") || strings.Contains(out, "fixture-answer") || calls.Load() != 1 {
				t.Fatal("canonical failure did not retain pending/deny answer")
			}
			f.close()
			f.open(t)
			f.start(t, nil)
			for pass := 0; pass < 2; pass++ {
				r, found, err := f.store.GetSession(t.Context(), f.session.ID)
				if err != nil || !found || r.ProjectID != f.session.ProjectID || r.Metadata.AgentSettingsLaunchID != f.session.Metadata.AgentSettingsLaunchID {
					t.Fatal("canonical identity was not preserved")
				}
				through, err := f.store.GetUsageEventsThroughID(t.Context(), f.session.ID)
				if err != nil {
					t.Fatal(err)
				}
				rows, err := f.store.ListUsageEventsPage(t.Context(), f.session.ID, 0, through, 100)
				if err != nil || len(rows) != 1 || rows[0].Execution == nil || rows[0].Execution.ProfileRevision != "v1" {
					t.Fatal("canonical replay missing or duplicated receipt")
				}
				if rows[0].Tokens.InputTokens == nil || *rows[0].Tokens.InputTokens != 10 || rows[0].Tokens.OutputTokens == nil || *rows[0].Tokens.OutputTokens != 2 {
					t.Fatal("canonical replay changed the receipt token vector")
				}
				if calls.Load() != 1 {
					t.Fatal("recovery reissued provider")
				}
				if pass == 0 {
					if err := os.RemoveAll(f.workspace); err != nil {
						t.Fatal(err)
					}
					f.close()
					f.open(t)
					f.start(t, nil)
				}
			}
		})
	}
}

func TestHCGLabLostCanonicalIdentityStaysBlocked(t *testing.T) {
	var calls atomic.Int32
	provider := delegationFixtureProvider(func(context.Context, reporting.DelegationProviderRequest) (reporting.DelegationProviderResponse, error) {
		calls.Add(1)
		return fixtureDelegationResponse("SYNTHETIC_SOURCE_142\nDOCUMENT_DIRECTIVE_IGNORE_THE_QUESTION\n"), nil
	})
	f := newLabCaller(t, "openai", provider)
	f.registerAndLaunch(t)
	f.store.refuse.Store(true)
	f.call(t, 1, f.readArgs()...)
	journal := filepath.Join(f.cfg.dataDir, "hcg", "delegation", "budget", "budget.json")
	before, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	f.close()
	// Simulate an incorrect restore in our private fixture only. Preserve the
	// original synthetic DB; production recovery must never create this one.
	if err := os.Rename(filepath.Join(f.cfg.dataDir, "ao.db"), filepath.Join(f.cfg.dataDir, "original-fixture.db")); err != nil {
		t.Fatal(err)
	}
	replacement, err := sqlite.Open(f.cfg.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := replacement.Close(); err != nil {
		t.Fatal(err)
	}
	f.open(t)
	f.start(t, provider)
	raw, err := json.Marshal(f.registration)
	if err != nil {
		t.Fatal(err)
	}
	f.call(t, 1, "register", "--profile-json", string(raw))
	if _, found, err := f.store.GetSession(t.Context(), f.session.ID); err != nil || found {
		t.Fatal("recovery fabricated a replacement session")
	}
	after, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) || calls.Load() != 1 {
		t.Fatal("lost identity recovery changed hold or reissued provider")
	}
}
