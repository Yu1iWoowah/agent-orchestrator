package agentaccounts

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agentaccount"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

type orphanBindingStore struct{ *memStore }

func (*orphanBindingStore) GetSessionAgentAccount(_ context.Context, _ domain.SessionID, p domain.AgentAccountProvider) (string, bool, error) {
	if p == domain.AgentAccountProviderCodex {
		return "deleted-codex", true, nil
	}
	return domain.DeviceAgentAccountID(p), true, nil
}
func TestLegacyOrphanBindingRemainsTraceableAndProviderScoped(t *testing.T) {
	s := New(&orphanBindingStore{&memStore{}}, []ports.AgentAccountProviderCLI{&fakeClaude{}, &evidenceCodex{fakeClaude: &fakeClaude{}}}, filepath.Join(t.TempDir(), "accounts"), nil, nil)
	ctx := context.Background()
	a, bound, e := s.SessionAccount(ctx, "old", domain.AgentAccountProviderCodex)
	if e != nil || !bound || a.ID != "deleted-codex" || a.RetiredAt == nil || a.Device() {
		t.Fatalf("orphan binding lost retired identity: %+v %t %v", a, bound, e)
	}
	if id, e := s.SessionAccountID(ctx, "old", domain.HarnessCodex); e != nil || id != a.ID {
		t.Fatalf("historical account ID: %s %v", id, e)
	}
	if ids, e := s.SessionAccounts(ctx, "old"); e != nil || ids[domain.AgentAccountProviderCodex] != a.ID {
		t.Fatalf("session account projection: %v %v", ids, e)
	}
	env, e := s.SessionLaunchEnv(ctx, "old", domain.HarnessClaudeCode)
	if e != nil || env["CODEX_HOME"] != "" || env["AO_CODEX_MANAGED_ACCOUNT"] != "" {
		t.Fatalf("inactive orphan blocked launch or pinned unknown home: %v %v", env, e)
	}
	if _, e = s.SessionLaunchEnv(ctx, "old", domain.HarnessCodex); !errors.Is(e, ports.ErrAgentAccountUnknown) {
		t.Fatalf("active orphan launched: %v", e)
	}
}
func TestDisabledDefaultFallsBackForNewSessionAndReadiness(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "accounts")
	now := time.Now()
	bad, good := filepath.Join(root, "claude", "bad"), filepath.Join(root, "claude", "good")
	for _, h := range []string{bad, good} {
		if e := ensureAccountProfile(root, h); e != nil {
			t.Fatal(e)
		}
	}
	st := &memStore{accounts: []domain.AgentAccount{{ID: "bad", Provider: domain.AgentAccountProviderClaude, Home: bad, DisabledAt: &now}, {ID: "good", Provider: domain.AgentAccountProviderClaude, Home: good, Email: "good@example.invalid"}}, defaults: map[domain.AgentAccountProvider]string{domain.AgentAccountProviderClaude: "bad"}, bindings: map[domain.SessionID]string{}}
	s := New(st, []ports.AgentAccountProviderCLI{&fakeClaude{signedInAs: map[string]string{good: "good@example.invalid"}}}, root, nil, nil)
	if e := s.BindSession(ctx, "new", domain.HarnessClaudeCode, ""); e != nil || st.bindings["new"] != "good" {
		t.Fatalf("disabled default did not fall back: %v %v", st.bindings, e)
	}
	id, handled := s.DefaultSignIn(ctx, domain.HarnessClaudeCode)
	if !handled || !id.SignedIn || id.Email != "good@example.invalid" {
		t.Fatalf("readiness disagrees with launch: %+v %t", id, handled)
	}
	if e := s.ValidateAccount(ctx, domain.HarnessClaudeCode, "bad"); !errors.Is(e, ports.ErrAgentAccountSignedOut) {
		t.Fatalf("explicit disabled selection fell back: %v", e)
	}
}
func TestExitedSuccessfulLoginIsRecoveredAndVerifyCannotLoseIt(t *testing.T) {
	for _, concurrent := range []bool{false, true} {
		t.Run(map[bool]string{false: "without_settings", true: "during_verify"}[concurrent], func(t *testing.T) {
			ctx := context.Background()
			db := sqlitetest.MustOpen(t)
			root := filepath.Join(t.TempDir(), "accounts")
			p := &raceIdentityProvider{fakeClaude: &fakeClaude{}, entered: make(chan struct{}), release: make(chan struct{})}
			term := &lifecycleTerminals{alive: map[string]bool{}}
			s := New(db, []ports.AgentAccountProviderCLI{p}, root, term, nil)
			start, e := s.StartLogin(ctx, domain.AgentAccountProviderClaude)
			if e != nil {
				t.Fatal(e)
			}
			p.target = filepath.Join(root, "claude", start.OperationID)
			term.alive[start.Terminal.HandleID] = false
			verified := make(chan error, 1)
			if concurrent {
				p.pause.Store(true)
				go func() { _, _, e := s.VerifyLogin(ctx, start.OperationID); verified <- e }()
				<-p.entered
			}
			recovered := make(chan error, 1)
			go func() { recovered <- s.Recover(ctx) }()
			if concurrent {
				select {
				case e := <-recovered:
					recovered <- e
				case <-time.After(30 * time.Millisecond):
				}
				close(p.release)
				if e := <-verified; e != nil {
					t.Errorf("successful login lost during verify: %v", e)
				}
			}
			if e := <-recovered; e != nil {
				t.Fatal(e)
			}
			o, e := s.Operation(ctx, start.OperationID)
			if e != nil || o.Phase != "complete" || o.Outcome != "complete" {
				t.Fatalf("successful child exit treated as cancel: %+v %v", o, e)
			}
			if _, e := os.Stat(p.target); e != nil {
				t.Fatal("recovery deleted successful login")
			}
		})
	}
}

type fileLogoutProvider struct {
	*fakeClaude
	calls            int
	failAfterRemoval bool
}

func (*fileLogoutProvider) Provider() domain.AgentAccountProvider {
	return domain.AgentAccountProviderCodex
}
func (*fileLogoutProvider) Identity(_ context.Context, home string) (domain.AgentAccountIdentityCheck, error) {
	if home == "" {
		return domain.AgentAccountIdentityCheck{}, nil
	}
	_, e := os.Lstat(filepath.Join(home, "auth.json"))
	if errors.Is(e, os.ErrNotExist) {
		return domain.AgentAccountIdentityCheck{}, nil
	}
	if e != nil {
		return domain.AgentAccountIdentityCheck{}, e
	}
	return domain.AgentAccountIdentityCheck{LoggedIn: true, SignedIn: true, Email: "fake@example.invalid"}, nil
}
func (p *fileLogoutProvider) Logout(_ context.Context, home string) error {
	p.calls++
	if _, e := os.Lstat(filepath.Join(home, "auth.json")); e != nil {
		return ports.ErrAgentAccountCredentialStoreUnsupported
	}
	if e := os.Remove(filepath.Join(home, "auth.json")); e != nil {
		return e
	}
	if p.failAfterRemoval {
		return errors.New("native exited after logout without acknowledgement")
	}
	return nil
}
func TestCodexLogoutAbsenceIsConfirmedBeforeRemoveAndRetry(t *testing.T) {
	for _, scenario := range []string{"logout_then_remove", "retry_after_native_success", "already_logged_out"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			db := sqlitetest.MustOpen(t)
			root := filepath.Join(t.TempDir(), "accounts")
			home := filepath.Join(root, "codex", "saved")
			if e := ensureAccountProfile(root, home); e != nil {
				t.Fatal(e)
			}
			if scenario != "already_logged_out" {
				if e := os.WriteFile(filepath.Join(home, "auth.json"), []byte("fake"), 0600); e != nil {
					t.Fatal(e)
				}
			}
			if e := db.CreateAgentAccount(ctx, domain.AgentAccount{ID: "saved", Provider: domain.AgentAccountProviderCodex, Home: home, CreatedAt: time.Now()}); e != nil {
				t.Fatal(e)
			}
			p := &fileLogoutProvider{fakeClaude: &fakeClaude{}, failAfterRemoval: scenario == "retry_after_native_success"}
			s := New(db, []ports.AgentAccountProviderCLI{p}, root, nil, nil)
			switch scenario {
			case "retry_after_native_success":
				if e := s.Logout(ctx, "saved"); !errors.Is(e, ports.ErrAgentAccountCleanupUnconfirmed) {
					t.Fatalf("lost uncertain native result: %v", e)
				}
				if e := s.Logout(ctx, "saved"); e != nil {
					t.Fatalf("logout retry rejected missing evidence: %v", e)
				}
			case "logout_then_remove":
				if e := s.Logout(ctx, "saved"); e != nil {
					t.Fatal(e)
				}
			}
			if e := s.Remove(ctx, "saved"); e != nil {
				t.Fatalf("removing signed-out account: %v", e)
			}
			want := 1
			if scenario == "already_logged_out" {
				want = 0
			}
			if p.calls != want {
				t.Fatalf("native logout calls=%d want %d", p.calls, want)
			}
		})
	}
}
func TestAcceptedDisabledIdentityCannotBeSavedTwice(t *testing.T) {
	ctx := context.Background()
	db := sqlitetest.MustOpen(t)
	root := filepath.Join(t.TempDir(), "accounts")
	p := &raceIdentityProvider{fakeClaude: &fakeClaude{}}
	term := &lifecycleTerminals{alive: map[string]bool{}}
	s := New(db, []ports.AgentAccountProviderCLI{p}, root, term, nil)
	first, e := s.StartLogin(ctx, domain.AgentAccountProviderClaude)
	if e != nil {
		t.Fatal(e)
	}
	second, e := s.StartLogin(ctx, domain.AgentAccountProviderClaude)
	if e != nil {
		t.Fatal(e)
	}
	term.closeErr = errors.New("writer close unconfirmed")
	if _, _, e = s.VerifyLogin(ctx, first.OperationID); !errors.Is(e, ports.ErrAgentAccountCleanupUnconfirmed) {
		t.Fatal(e)
	}
	term.closeErr = nil
	status, view, e := s.VerifyLogin(ctx, second.OperationID)
	if e != nil || status != LoginDuplicate || view == nil || view.Account.ID != first.OperationID {
		t.Fatalf("accepted identity duplicated: %s %+v %v", status, view, e)
	}
	rows, _ := db.ListAgentAccounts(ctx)
	if len(rows) != 1 || rows[0].DisabledAt == nil {
		t.Fatal("unconfirmed accepted row lost duplicate protection")
	}
}

type blockedPrepareProvider struct {
	*fakeClaude
	pause            atomic.Bool
	entered, release chan struct{}
}

func (p *blockedPrepareProvider) Prepare(home string) error {
	if p.pause.CompareAndSwap(true, false) {
		close(p.entered)
		<-p.release
	}
	return (&agentaccount.Claude{}).Prepare(home)
}
func TestCancelWaitsForPrepareAndLeavesNoRecreatedProfile(t *testing.T) {
	ctx := context.Background()
	db := sqlitetest.MustOpen(t)
	root := filepath.Join(t.TempDir(), "accounts")
	p := &blockedPrepareProvider{fakeClaude: &fakeClaude{}, entered: make(chan struct{}), release: make(chan struct{})}
	s := New(db, []ports.AgentAccountProviderCLI{p}, root, &lifecycleTerminals{alive: map[string]bool{}}, nil)
	start, e := s.StartLogin(ctx, domain.AgentAccountProviderClaude)
	if e != nil {
		t.Fatal(e)
	}
	p.pause.Store(true)
	verified := make(chan error, 1)
	go func() { _, _, e := s.VerifyLogin(ctx, start.OperationID); verified <- e }()
	<-p.entered
	cancelled := make(chan error, 1)
	go func() { cancelled <- s.CancelLogin(ctx, start.OperationID) }()
	early := false
	select {
	case e := <-cancelled:
		early = true
		cancelled <- e
	case <-time.After(30 * time.Millisecond):
	}
	close(p.release)
	if e := <-verified; e != nil {
		t.Fatal(e)
	}
	if e := <-cancelled; e != nil {
		t.Fatal(e)
	}
	if e := s.Recover(ctx); e != nil {
		t.Fatal(e)
	}
	if _, e := os.Stat(filepath.Join(root, "claude", start.OperationID)); !errors.Is(e, os.ErrNotExist) || early {
		t.Fatal("cancel finished before Prepare and left recreated orphan profile")
	}
}

type countedJournal struct {
	ports.AgentAccountStore
	ports.AgentAccountJournal
	writes int
}

func (j *countedJournal) UpdateAgentAccountOperation(ctx context.Context, o domain.AgentAccountOperation) error {
	j.writes++
	return j.AgentAccountJournal.UpdateAgentAccountOperation(ctx, o)
}
func TestFailedRecoveryBacksOffWithoutUnchangedCDC(t *testing.T) {
	ctx := context.Background()
	db := sqlitetest.MustOpen(t)
	root := filepath.Join(t.TempDir(), "accounts")
	home := filepath.Join(root, "claude", "saved")
	if e := ensureAccountProfile(root, home); e != nil {
		t.Fatal(e)
	}
	if e := db.CreateAgentAccount(ctx, domain.AgentAccount{ID: "saved", Provider: domain.AgentAccountProviderClaude, Home: home, CreatedAt: time.Now()}); e != nil {
		t.Fatal(e)
	}
	j := &countedJournal{AgentAccountStore: db, AgentAccountJournal: db}
	p := &lifecycleProvider{fakeClaude: &fakeClaude{signedInAs: map[string]string{home: "fake@example.invalid"}}, logoutErr: errors.New("unconfirmed")}
	calls := 0
	s := New(j, []ports.AgentAccountProviderCLI{p}, root, nil, nil)
	s.SetAgentStopper(func(context.Context, string) error { calls++; return nil })
	now := time.Now().UTC()
	s.now = func() time.Time { return now }
	if e := s.Logout(ctx, "saved"); !errors.Is(e, ports.ErrAgentAccountCleanupUnconfirmed) {
		t.Fatal(e)
	}
	writes := j.writes
	for range 3 {
		_ = s.Recover(ctx)
	}
	if calls != 1 || j.writes != writes {
		t.Fatalf("no backoff: native/stop calls=%d writes=%d initial=%d", calls, j.writes, writes)
	}
	now = now.Add(2 * time.Minute)
	_ = s.Recover(ctx)
	if calls != 2 || j.writes != writes {
		t.Fatalf("retry rewrote unchanged phase/reason: calls=%d writes=%d initial=%d", calls, j.writes, writes)
	}
}

func TestRecoverCleansRecreatedProfileOfCompletedCancellation(t *testing.T) {
	ctx := context.Background()
	db := sqlitetest.MustOpen(t)
	root := filepath.Join(t.TempDir(), "accounts")
	s := New(db, []ports.AgentAccountProviderCLI{&fakeClaude{}}, root, &lifecycleTerminals{alive: map[string]bool{}}, nil)
	start, e := s.StartLogin(ctx, domain.AgentAccountProviderClaude)
	if e != nil {
		t.Fatal(e)
	}
	if e := s.CancelLogin(ctx, start.OperationID); e != nil {
		t.Fatal(e)
	}
	home := filepath.Join(root, "claude", start.OperationID)
	if e := ensureAccountProfile(root, home); e != nil {
		t.Fatal(e)
	}
	restarted := New(db, []ports.AgentAccountProviderCLI{&fakeClaude{}}, root, &lifecycleTerminals{alive: map[string]bool{}}, nil)
	if e := restarted.Recover(ctx); e != nil {
		t.Fatal(e)
	}
	if _, e := os.Stat(home); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("completed cancelled operation permanently skipped recreated profile")
	}
}

type oneUnconfirmedTerminal struct {
	*lifecycleTerminals
	first string
}

func (t *oneUnconfirmedTerminal) CloseCommandTerminal(ctx context.Context, id string) error {
	if id == t.first {
		return errors.New("first writer still alive")
	}
	return t.lifecycleTerminals.CloseCommandTerminal(ctx, id)
}
func TestConcurrentVerifyProtectsUnconfirmedAcceptedIdentity(t *testing.T) {
	ctx := context.Background()
	db := sqlitetest.MustOpen(t)
	root := filepath.Join(t.TempDir(), "accounts")
	p := &raceIdentityProvider{fakeClaude: &fakeClaude{}, entered: make(chan struct{}), release: make(chan struct{})}
	term := &oneUnconfirmedTerminal{lifecycleTerminals: &lifecycleTerminals{alive: map[string]bool{}}}
	s := New(db, []ports.AgentAccountProviderCLI{p}, root, term, nil)
	first, e := s.StartLogin(ctx, domain.AgentAccountProviderClaude)
	if e != nil {
		t.Fatal(e)
	}
	second, e := s.StartLogin(ctx, domain.AgentAccountProviderClaude)
	if e != nil {
		t.Fatal(e)
	}
	term.first = first.Terminal.HandleID
	p.target = filepath.Join(root, "claude", first.OperationID)
	p.pause.Store(true)
	firstDone := make(chan error, 1)
	go func() { _, _, e := s.VerifyLogin(ctx, first.OperationID); firstDone <- e }()
	<-p.entered
	secondDone := make(chan error, 1)
	go func() {
		status, view, e := s.VerifyLogin(ctx, second.OperationID)
		if e == nil && (status != LoginDuplicate || view == nil || view.Account.ID != first.OperationID) {
			e = errors.New("second operation accepted already reserved identity")
		}
		secondDone <- e
	}()
	close(p.release)
	if e := <-firstDone; !errors.Is(e, ports.ErrAgentAccountCleanupUnconfirmed) {
		t.Fatal(e)
	}
	if e := <-secondDone; e != nil {
		t.Fatal(e)
	}
	rows, e := db.ListAgentAccounts(ctx)
	if e != nil || len(rows) != 1 || rows[0].DisabledAt == nil {
		t.Fatal("concurrent acceptance bypassed disabled identity reservation")
	}
}
func TestStopFailureBackoffPreservesScheduledRemoval(t *testing.T) {
	ctx := context.Background()
	db := sqlitetest.MustOpen(t)
	root := filepath.Join(t.TempDir(), "accounts")
	home := filepath.Join(root, "claude", "saved")
	if e := ensureAccountProfile(root, home); e != nil {
		t.Fatal(e)
	}
	if e := db.CreateAgentAccount(ctx, domain.AgentAccount{ID: "saved", Provider: domain.AgentAccountProviderClaude, Home: home, CreatedAt: time.Now()}); e != nil {
		t.Fatal(e)
	}
	j := &countedJournal{AgentAccountStore: db, AgentAccountJournal: db}
	s := New(j, []ports.AgentAccountProviderCLI{&fakeClaude{}}, root, nil, nil)
	calls := 0
	s.SetAgentStopper(func(context.Context, string) error { calls++; return errors.New("agent busy") })
	now := time.Now().UTC()
	s.now = func() time.Time { return now }
	e := s.Remove(ctx, "saved")
	var opErr *OperationError
	if !errors.Is(e, ports.ErrAgentAccountInUse) || !errors.As(e, &opErr) || opErr.OperationID == "" {
		t.Fatalf("scheduled removal lost operation reference: %v", e)
	}
	writes := j.writes
	_ = s.Recover(ctx)
	if calls != 1 || j.writes != writes {
		t.Fatal("stop failure retried without backoff")
	}
	now = now.Add(2 * time.Minute)
	_ = s.Recover(ctx)
	if calls != 2 || j.writes != writes {
		t.Fatal("unchanged stop failure emitted another journal update")
	}
	rows, _ := db.ListAgentAccounts(ctx)
	if len(rows) != 1 || rows[0].DisabledAt == nil || rows[0].RetiredAt != nil {
		t.Fatal("scheduled removal did not preserve disabled account")
	}
}

func TestExitedUnsignedLoginIsCleanedOnlyAfterNativeProbe(t *testing.T) {
	ctx := context.Background()
	db := sqlitetest.MustOpen(t)
	root := filepath.Join(t.TempDir(), "accounts")
	p := &fakeClaude{}
	term := &lifecycleTerminals{alive: map[string]bool{}}
	s := New(db, []ports.AgentAccountProviderCLI{p}, root, term, nil)
	start, e := s.StartLogin(ctx, domain.AgentAccountProviderClaude)
	if e != nil {
		t.Fatal(e)
	}
	term.alive[start.Terminal.HandleID] = false
	if e := s.Recover(ctx); e != nil {
		t.Fatal(e)
	}
	o, e := s.Operation(ctx, start.OperationID)
	if e != nil || o.Phase != "complete" || o.Outcome != "cancelled" {
		t.Fatalf("confirmed unsigned child exit never converged: %+v %v", o, e)
	}
	if _, e := os.Stat(filepath.Join(root, "claude", start.OperationID)); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("unsigned child left profile")
	}
}
