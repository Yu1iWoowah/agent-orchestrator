package agentaccounts

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/shellterm"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

type lifecycleTerminals struct {
	mu       sync.Mutex
	alive    map[string]bool
	closeErr error
}

func (t *lifecycleTerminals) OpenCommandTerminal(_ context.Context, in shellterm.OpenCommandTerminalInput) (shellterm.ShellTerminal, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	id := in.HandleID
	t.alive[id] = true
	return shellterm.ShellTerminal{HandleID: id}, nil
}
func (t *lifecycleTerminals) CloseShellTerminal(_ context.Context, id string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closeErr != nil {
		return t.closeErr
	}
	delete(t.alive, id)
	return nil
}
func (t *lifecycleTerminals) CloseCommandTerminal(ctx context.Context, id string) error {
	return t.CloseShellTerminal(ctx, id)
}

type lifecycleProvider struct {
	*fakeClaude
	logoutErr error
}

func (p *lifecycleProvider) Logout(ctx context.Context, home string) error {
	if p.logoutErr != nil {
		return p.logoutErr
	}
	return p.fakeClaude.Logout(ctx, home)
}

func TestLoginCompletionIsDurableAndRepeatable(t *testing.T) {
	ctx := context.Background()
	store := sqlitetest.MustOpen(t)
	root := filepath.Join(t.TempDir(), "accounts")
	p := &fakeClaude{signedInAs: map[string]string{}}
	term := &lifecycleTerminals{alive: map[string]bool{}}
	svc := New(store, []ports.AgentAccountProviderCLI{p}, root, term, nil)
	start, err := svc.StartLogin(ctx, domain.AgentAccountProviderClaude)
	if err != nil {
		t.Fatalf("start=%v journal=%T", err, store)
	}
	p.signedInAs[filepath.Join(root, "claude", start.OperationID)] = "fake@example.invalid"
	status, view, err := svc.VerifyLogin(ctx, start.OperationID)
	if err != nil || status != LoginComplete || view == nil {
		t.Fatalf("first verify: %s %v", status, err)
	}
	restarted := New(store, []ports.AgentAccountProviderCLI{p}, root, term, nil)
	again, got, err := restarted.VerifyLogin(ctx, start.OperationID)
	if err != nil || again != LoginComplete || got == nil || got.Account.ID != view.Account.ID {
		t.Fatalf("repeat verify after restart: %s %v", again, err)
	}
	rows, err := store.ListAgentAccounts(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatal("repeat verify duplicated account")
	}
}

func TestCancelSurvivesRestartAndDoesNotDeleteLiveProfile(t *testing.T) {
	ctx := context.Background()
	store := sqlitetest.MustOpen(t)
	root := filepath.Join(t.TempDir(), "accounts")
	p := &fakeClaude{signedInAs: map[string]string{}}
	term := &lifecycleTerminals{alive: map[string]bool{}}
	svc := New(store, []ports.AgentAccountProviderCLI{p}, root, term, nil)
	start, err := svc.StartLogin(ctx, domain.AgentAccountProviderClaude)
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "claude", start.OperationID)
	term.closeErr = errors.New("fake-private-secret close failed")
	restarted := New(store, []ports.AgentAccountProviderCLI{p}, root, term, nil)
	if err := restarted.CancelLogin(ctx, start.OperationID); err == nil {
		t.Fatal("cancel claims success while terminal survives")
	}
	if _, err := os.Stat(home); err != nil {
		t.Fatal("cancel removed profile still used by live terminal")
	}
	term.closeErr = nil
	if err := restarted.CancelLogin(ctx, start.OperationID); err != nil {
		t.Fatalf("retry cancel after restart: %v", err)
	}
	if _, err := os.Stat(home); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cancel did not converge profile cleanup")
	}
	if err := restarted.CancelLogin(ctx, start.OperationID); err != nil {
		t.Fatal("repeat cancel is not idempotent")
	}
}

func TestLogoutFailureIsDurableAndBlocksUseUntilRetry(t *testing.T) {
	ctx := context.Background()
	store := sqlitetest.MustOpen(t)
	root := filepath.Join(t.TempDir(), "accounts")
	home := filepath.Join(root, "claude", "saved")
	if e := ensureAccountProfile(root, home); e != nil {
		t.Fatal(e)
	}
	p := &lifecycleProvider{fakeClaude: &fakeClaude{signedInAs: map[string]string{home: "fake@example.invalid"}}, logoutErr: errors.New("fake-credential-marker logout rejected")}
	if e := store.CreateAgentAccount(ctx, domain.AgentAccount{ID: "saved", Provider: domain.AgentAccountProviderClaude, Home: home, CreatedAt: time.Now()}); e != nil {
		t.Fatal(e)
	}
	svc := New(store, []ports.AgentAccountProviderCLI{p}, root, &lifecycleTerminals{alive: map[string]bool{}}, nil)
	if e := svc.SetDefault(ctx, "saved"); e != nil {
		t.Fatal(e)
	}
	if _, e := svc.List(ctx, true); e != nil {
		t.Fatal(e)
	}
	if e := svc.Logout(ctx, "saved"); !errors.Is(e, ports.ErrAgentAccountCleanupUnconfirmed) {
		t.Fatalf("logout=%v", e)
	}
	if identity, handled := svc.DefaultSignIn(ctx, domain.HarnessClaudeCode); identity.LoggedIn || !handled {
		t.Fatal("pending logout still reads as ready from cached login")
	}
	ops, e := store.ListAgentAccountOperations(ctx)
	if e != nil || len(ops) != 1 || ops[0].ReasonCode != "account_cleanup_unconfirmed" || ops[0].Phase == "complete" {
		t.Fatal("uncertain logout was not retained")
	}
	if strings.Contains(ops[0].ReasonCode, "fake-credential-marker") {
		t.Fatal("provider error reached journal")
	}
	if e := svc.ValidateAccount(ctx, domain.HarnessClaudeCode, "saved"); !errors.Is(e, ports.ErrAgentAccountSignedOut) {
		t.Fatalf("pending logout still launches: %v", e)
	}
	if _, e := os.Stat(home); e != nil {
		t.Fatal("failed logout removed profile")
	}
	p.logoutErr = nil
	restarted := New(store, []ports.AgentAccountProviderCLI{p}, root, nil, nil)
	if e := restarted.Recover(ctx); e != nil {
		t.Fatal(e)
	}
	ops, _ = store.ListAgentAccountOperations(ctx)
	if len(ops) != 1 || ops[0].Phase != "complete" || ops[0].ReasonCode != "" {
		t.Fatal("logout retry did not converge same operation")
	}
	rows, _ := store.ListAgentAccounts(ctx)
	if len(rows) != 1 || rows[0].DisabledAt == nil || rows[0].RetiredAt != nil {
		t.Fatal("logout lost metadata or allowed new use")
	}
}

func TestRetiredAccountKeepsMetadataHistoryAndBinding(t *testing.T) {
	ctx := context.Background()
	store := &memStore{defaults: map[domain.AgentAccountProvider]string{}, bindings: map[domain.SessionID]string{"old": "saved"}}
	root := filepath.Join(t.TempDir(), "accounts")
	home := filepath.Join(root, "claude", "saved")
	if e := ensureAccountProfile(root, home); e != nil {
		t.Fatal(e)
	}
	history := filepath.Join(root, "history")
	if e := os.Mkdir(history, 0700); e != nil {
		t.Fatal(e)
	}
	sentinel := filepath.Join(history, "keep")
	if e := os.WriteFile(sentinel, []byte("fake-history"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink(history, filepath.Join(home, "projects")); e != nil {
		t.Fatal(e)
	}
	store.accounts = []domain.AgentAccount{{ID: "saved", Provider: domain.AgentAccountProviderClaude, Home: home, Email: "fake@example.invalid", Label: "Fake account", CreatedAt: time.Now()}}
	svc := New(store, []ports.AgentAccountProviderCLI{&fakeClaude{}}, root, nil, nil)
	if e := svc.Remove(ctx, "saved"); e != nil {
		t.Fatal(e)
	}
	if len(store.accounts) != 1 || store.accounts[0].RetiredAt == nil || store.accounts[0].Label != "Fake account" {
		t.Fatal("remove erased historical account metadata")
	}
	if store.bindings["old"] != "saved" {
		t.Fatal("remove rewrote historical binding")
	}
	if e := svc.BindSession(ctx, "old", domain.HarnessClaudeCode, "saved"); !errors.Is(e, ports.ErrAgentAccountUnknown) {
		t.Fatalf("retired account was selectable: %v", e)
	}
	_, e := svc.SessionLaunchEnv(ctx, "old", domain.HarnessClaudeCode)
	if !errors.Is(e, ports.ErrAgentAccountUnknown) || store.bindings["old"] != "saved" {
		t.Fatal("retired binding fell back at restore")
	}
	if data, e := os.ReadFile(sentinel); e != nil || string(data) != "fake-history" {
		t.Fatal("cleanup followed shared history link")
	}
	if _, e := os.Stat(home); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("profile cleanup incomplete")
	}
}

type crashJournal struct {
	ports.AgentAccountStore
	ports.AgentAccountJournal
	phase string
	crash any
}

func (j *crashJournal) CreateAgentAccountOperation(c context.Context, o domain.AgentAccountOperation) error {
	e := j.AgentAccountJournal.CreateAgentAccountOperation(c, o)
	if e == nil && o.Phase == j.phase {
		panic(j.crash)
	}
	return e
}
func (j *crashJournal) UpdateAgentAccountOperation(c context.Context, o domain.AgentAccountOperation) error {
	e := j.AgentAccountJournal.UpdateAgentAccountOperation(c, o)
	if e == nil && o.Phase == j.phase {
		panic(j.crash)
	}
	return e
}
func (j *crashJournal) CommitAgentAccountLogin(c context.Context, o domain.AgentAccountOperation, a domain.AgentAccount) error {
	e := j.AgentAccountJournal.CommitAgentAccountLogin(c, o, a)
	if e == nil && o.Phase == j.phase {
		panic(j.crash)
	}
	return e
}

func TestLifecycleRecoversEveryPersistedCrashPhase(t *testing.T) {
	for _, phase := range []string{"pending", "accepted", "closing", "logging-out", "deleting", "complete"} {
		t.Run(phase, func(t *testing.T) {
			ctx := context.Background()
			db := sqlitetest.MustOpen(t)
			root := filepath.Join(t.TempDir(), "accounts")
			p := &fakeClaude{signedInAs: map[string]string{}}
			term := &lifecycleTerminals{alive: map[string]bool{}}
			crash := errors.New("simulated process exit")
			j := &crashJournal{AgentAccountStore: db, AgentAccountJournal: db, crash: crash}
			svc := New(j, []ports.AgentAccountProviderCLI{p}, root, term, nil)
			var start LoginStart
			var err error
			if phase != "pending" {
				start, err = svc.StartLogin(ctx, domain.AgentAccountProviderClaude)
				if err != nil {
					t.Fatal(err)
				}
			}
			j.phase = phase
			func() {
				defer func() {
					if got, ok := recover().(error); !ok || !errors.Is(got, crash) {
						t.Fatalf("did not reach durable crash phase: %v", got)
					}
				}()
				switch phase {
				case "pending":
					_, _ = svc.StartLogin(ctx, domain.AgentAccountProviderClaude)
				case "accepted":
					p.signedInAs[filepath.Join(root, "claude", start.OperationID)] = "fake@example.invalid"
					_, _, _ = svc.VerifyLogin(ctx, start.OperationID)
				default:
					_ = svc.CancelLogin(ctx, start.OperationID)
				}
			}()
			restarted := New(db, []ports.AgentAccountProviderCLI{p}, root, term, nil)
			if err := restarted.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			if err := restarted.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			ops, err := db.ListAgentAccountOperations(ctx)
			if err != nil || len(ops) != 1 || ops[0].Phase != "complete" {
				t.Fatal("crash recovery did not converge journal")
			}
			if len(term.alive) != 0 {
				t.Fatal("crash recovery left a trusted writer")
			}
			rows, _ := db.ListAgentAccounts(ctx)
			if phase == "accepted" {
				if len(rows) != 1 || rows[0].DisabledAt != nil {
					t.Fatal("accepted account was deleted or stayed disabled")
				}
				status, view, e := restarted.VerifyLogin(ctx, ops[0].ID)
				if e != nil || status != LoginComplete || view == nil {
					t.Fatal("accepted result is not replayable")
				}
			} else {
				if len(rows) != 0 {
					t.Fatal("cancelled operation saved account")
				}
				if _, e := os.Stat(filepath.Join(root, "claude", ops[0].AccountID)); !errors.Is(e, os.ErrNotExist) {
					t.Fatal("cancelled profile survived crash cleanup")
				}
			}
		})
	}
}

type deletionFailureProvider struct {
	*fakeClaude
	calls int
}

func (p *deletionFailureProvider) Logout(_ context.Context, home string) error {
	p.calls++
	return os.Chmod(filepath.Join(home, "locked"), 0500)
}
func TestDeletionRetryDoesNotRepeatCompletedLogout(t *testing.T) {
	ctx := context.Background()
	db := sqlitetest.MustOpen(t)
	root := filepath.Join(t.TempDir(), "accounts")
	home := filepath.Join(root, "claude", "saved")
	if e := ensureAccountProfile(root, home); e != nil {
		t.Fatal(e)
	}
	locked := filepath.Join(home, "locked")
	if e := os.Mkdir(locked, 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(locked, "fake-cache"), []byte("fake"), 0600); e != nil {
		t.Fatal(e)
	}
	defer func() { _ = os.Chmod(locked, 0700) }()
	if e := db.CreateAgentAccount(ctx, domain.AgentAccount{ID: "saved", Provider: domain.AgentAccountProviderClaude, Home: home, CreatedAt: time.Now()}); e != nil {
		t.Fatal(e)
	}
	p := &deletionFailureProvider{fakeClaude: &fakeClaude{signedInAs: map[string]string{home: "fake@example.invalid"}}}
	s := New(db, []ports.AgentAccountProviderCLI{p}, root, nil, nil)
	if e := s.Remove(ctx, "saved"); !errors.Is(e, ports.ErrAgentAccountCleanupUnconfirmed) {
		t.Fatalf("delete failure=%v", e)
	}
	ops, _ := db.ListAgentAccountOperations(ctx)
	if len(ops) != 1 || ops[0].Phase != "deleting" || ops[0].ReasonCode != "account_storage_unsafe" {
		t.Fatalf("delete checkpoint: phase=%s reason=%s", ops[0].Phase, ops[0].ReasonCode)
	}
	if e := os.Chmod(locked, 0700); e != nil {
		t.Fatal(e)
	}
	// Background retries back off; explicit user retries need not wait.
	if e := s.Remove(ctx, "saved"); e != nil {
		t.Fatal(e)
	}
	if p.calls != 1 {
		t.Fatal("local cleanup retry repeated completed native logout")
	}
}

type raceIdentityProvider struct {
	*fakeClaude
	target  string
	pause   atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (p *raceIdentityProvider) Identity(_ context.Context, home string) (domain.AgentAccountIdentityCheck, error) {
	if home == p.target && p.pause.CompareAndSwap(true, false) {
		close(p.entered)
		<-p.release
	}
	if home == "" {
		return domain.AgentAccountIdentityCheck{}, nil
	}
	return domain.AgentAccountIdentityCheck{SignedIn: true, LoggedIn: true, Email: "fake@example.invalid"}, nil
}
func TestCancelWaitsForNativeVerifyAndKeepsAcceptedResult(t *testing.T) {
	for range 5 {
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
		p.pause.Store(true)
		done := make(chan error, 1)
		go func() { _, _, e := s.VerifyLogin(ctx, start.OperationID); done <- e }()
		<-p.entered
		cancelled := make(chan error, 1)
		go func() { cancelled <- s.CancelLogin(ctx, start.OperationID) }()
		early := false
		select {
		case e := <-cancelled:
			early = true
			cancelled <- e
		case <-time.After(20 * time.Millisecond):
		}
		close(p.release)
		if e := <-done; e != nil {
			t.Fatal(e)
		}
		if e := <-cancelled; e != nil {
			t.Fatal(e)
		}
		rows, _ := db.ListAgentAccounts(ctx)
		if early || len(rows) != 1 || len(term.alive) != 0 {
			t.Fatal("cancel crossed native verify or erased accepted result")
		}
	}
}
func TestConcurrentVerifyCommitsOneAccountAndReplaysResult(t *testing.T) {
	ctx := context.Background()
	db := sqlitetest.MustOpen(t)
	root := filepath.Join(t.TempDir(), "accounts")
	p := &raceIdentityProvider{fakeClaude: &fakeClaude{}}
	s := New(db, []ports.AgentAccountProviderCLI{p}, root, &lifecycleTerminals{alive: map[string]bool{}}, nil)
	start, e := s.StartLogin(ctx, domain.AgentAccountProviderClaude)
	if e != nil {
		t.Fatal(e)
	}
	gate := make(chan struct{})
	done := make(chan error, 2)
	for range 2 {
		go func() {
			<-gate
			status, view, e := s.VerifyLogin(ctx, start.OperationID)
			if e == nil && (status != LoginComplete || view == nil || view.Account.ID != start.OperationID) {
				e = errors.New("verify result not replayed")
			}
			done <- e
		}()
	}
	close(gate)
	for range 2 {
		if e := <-done; e != nil {
			t.Fatal(e)
		}
	}
	rows, _ := db.ListAgentAccounts(ctx)
	if len(rows) != 1 {
		t.Fatal("concurrent verify duplicated account")
	}
}

func TestCompletedLogoutIsIdempotentAfterServiceRestart(t *testing.T) {
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
	p := &lifecycleProvider{fakeClaude: &fakeClaude{}}
	s := New(db, []ports.AgentAccountProviderCLI{p}, root, nil, nil)
	if e := s.Logout(ctx, "saved"); e != nil {
		t.Fatal(e)
	}
	p.logoutErr = errors.New("native credential file already removed")
	restarted := New(db, []ports.AgentAccountProviderCLI{p}, root, nil, nil)
	if e := restarted.Logout(ctx, "saved"); e != nil {
		t.Fatalf("completed logout invoked native command again: %v", e)
	}
	ops, _ := db.ListAgentAccountOperations(ctx)
	if len(ops) != 1 {
		t.Fatal("repeat completed logout created another operation")
	}
}

func (t *lifecycleTerminals) CommandTerminalAlive(_ context.Context, id string) (bool, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.alive[id], nil
}

type slowRecoveryProvider struct {
	*fakeClaude
	entered chan struct{}
	done    chan struct{}
}

func (p *slowRecoveryProvider) Logout(ctx context.Context, _ string) error {
	close(p.entered)
	<-ctx.Done()
	close(p.done)
	return ctx.Err()
}
func TestStartupRecoveryDoesNotBlockDaemonOnNativeLogout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db := sqlitetest.MustOpen(t)
	root := filepath.Join(t.TempDir(), "accounts")
	home := filepath.Join(root, "claude", "saved")
	if e := ensureAccountProfile(root, home); e != nil {
		t.Fatal(e)
	}
	now := time.Now()
	if e := db.CreateAgentAccount(ctx, domain.AgentAccount{ID: "saved", Provider: domain.AgentAccountProviderClaude, Home: home, CreatedAt: now}); e != nil {
		t.Fatal(e)
	}
	if e := db.CreateAgentAccountOperation(ctx, domain.AgentAccountOperation{ID: "logout", AccountID: "saved", Provider: domain.AgentAccountProviderClaude, Action: "logout", Phase: "logging-out", Outcome: "signed-out", CreatedAt: now, UpdatedAt: now}); e != nil {
		t.Fatal(e)
	}
	p := &slowRecoveryProvider{fakeClaude: &fakeClaude{signedInAs: map[string]string{home: "fake@example.invalid"}}, entered: make(chan struct{}), done: make(chan struct{})}
	s := New(db, []ports.AgentAccountProviderCLI{p}, root, nil, nil)
	returned := make(chan struct{})
	go func() { s.StartRecovery(ctx); close(returned) }()
	<-p.entered
	select {
	case <-returned:
	case <-time.After(100 * time.Millisecond):
		cancel()
		<-p.done
		<-returned
		t.Fatal("native recovery blocks daemon boot")
	}
	cancel()
	<-p.done
	s.opMu.Lock()
	settled, found, err := db.GetAgentAccountOperation(context.Background(), "logout")
	s.opMu.Unlock()
	if err != nil || !found || settled.ReasonCode != "account_cleanup_unconfirmed" {
		t.Fatal("cancelled recovery did not persist its uncertain native outcome")
	}

}

type duplicateRaceProvider struct{ *raceIdentityProvider }

func (p *duplicateRaceProvider) Identity(ctx context.Context, home string) (domain.AgentAccountIdentityCheck, error) {
	if home == "" {
		time.Sleep(20 * time.Millisecond)
	}
	return p.raceIdentityProvider.Identity(ctx, home)
}
func TestConcurrentLoginOperationsCannotSaveSameIdentityTwice(t *testing.T) {
	ctx := context.Background()
	db := sqlitetest.MustOpen(t)
	root := filepath.Join(t.TempDir(), "accounts")
	p := &duplicateRaceProvider{&raceIdentityProvider{fakeClaude: &fakeClaude{}}}
	s := New(db, []ports.AgentAccountProviderCLI{p}, root, &lifecycleTerminals{alive: map[string]bool{}}, nil)
	var starts []LoginStart
	for range 2 {
		start, e := s.StartLogin(ctx, domain.AgentAccountProviderClaude)
		if e != nil {
			t.Fatal(e)
		}
		starts = append(starts, start)
	}
	type result struct {
		status LoginStatus
		err    error
	}
	done := make(chan result, 2)
	gate := make(chan struct{})
	for _, start := range starts {
		go func() { <-gate; status, _, e := s.VerifyLogin(ctx, start.OperationID); done <- result{status, e} }()
	}
	close(gate)
	got := map[LoginStatus]int{}
	for range 2 {
		r := <-done
		if r.err != nil {
			t.Fatal(r.err)
		}
		got[r.status]++
	}
	rows, e := db.ListAgentAccounts(ctx)
	if e != nil || len(rows) != 1 || got[LoginComplete] != 1 || got[LoginDuplicate] != 1 {
		t.Fatal("concurrent operations saved the same native identity twice")
	}
}
