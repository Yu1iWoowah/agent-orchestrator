// Package agentaccounts manages the Claude Code and Codex subscription accounts
// AO launches agents with. An account is a config folder of the provider's own
// CLI; the computer's own folder is the "This computer" account. A session is
// pinned to one account per provider and moves by restarting its agent there.
package agentaccounts

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/agentlaunch"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/shellterm"
)

// freshFor is how long a read of an account's identity and limits is reused.
const freshFor = 5 * time.Minute

// retryEmptyAfter is how long an answer with no limit windows is reused. A
// folder read just after its sign-in can answer before its limits are known.
const retryEmptyAfter = 30 * time.Second

// retryFailedAfter is how long a check that failed on its own (a slow CLI, an
// unknown error) is reused. A busy computer's next check usually works; each
// failure in a row doubles the wait, up to freshFor, so a CLI that stays slow
// is not started every minute.
const retryFailedAfter = time.Minute

// Terminals opens the embedded terminal a sign-in runs in.
type Terminals interface {
	OpenCommandTerminal(context.Context, shellterm.OpenCommandTerminalInput) (shellterm.ShellTerminal, error)
	CloseCommandTerminal(context.Context, string) error
	CommandTerminalAlive(context.Context, string) (bool, error)
}

// resetCrediter is the Codex-only ability to spend a usage-limit reset.
type resetCrediter interface {
	ConsumeResetCredit(ctx context.Context, home, idempotencyKey string) (domain.CodexResetCreditOutcome, error)
}

// Service is the account catalog, its cached readings, and the per-session
// launch environment.
type Service struct {
	store          ports.AgentAccountStore
	providers      map[domain.AgentAccountProvider]ports.AgentAccountProviderCLI
	defaultHarness domain.AgentHarness
	root           string // account folders live in root/<provider>/<id>
	terminals      Terminals
	// stopAgents stops every running agent on an account before it is removed.
	stopAgents  func(context.Context, string) error
	usageOwners func(context.Context, string) ([]domain.AgentAccountUsageOwner, error)
	log         *slog.Logger
	now         func() time.Time

	mu                  sync.Mutex
	readings            map[string]*reading
	warnings            map[string]string
	activeLogins        map[string]bool
	retries             map[string]lifecycleRetry
	opMu                sync.Mutex
	useMu               sync.Mutex
	uses                map[string]*sync.RWMutex
	unavailableMu       sync.Mutex
	defaultChanged      func(domain.AgentAccountProvider)
	legacyIdentityCheck func(context.Context, domain.AgentAccountProvider, string) (bool, error)
	limitLog            ports.AgentLimitReadingStore
	readSeq             uint64
	lastOrg             map[string]seenOrg
	loginConfirmed      func(domain.AgentAccount, time.Time)
}

// reading is the last identity and limits read for one account.
type reading struct {
	identity    domain.AgentAccountIdentityCheck
	identityErr string
	// identityProblem and limitsProblem say what kind of failure each error is.
	identityProblem domain.AgentAccountProblemKind
	// mismatch: the folder is signed in, but as another login than its own.
	mismatch      bool
	limits        *domain.AgentAccountLimits
	limitsErr     string
	limitsProblem domain.AgentAccountProblemKind
	// failures counts checks in a row that failed on their own.
	failures     int
	at           time.Time
	seq          uint64
	confirmed    *domain.AgentAccountIdentityCheck
	confirmedSeq uint64
}

// failedOnItsOwn reports a reading whose check failed in a way the next check
// may not, such as a CLI too slow to answer.
func (r *reading) failedOnItsOwn() bool {
	transient := func(k domain.AgentAccountProblemKind) bool {
		return k.Transient() && k != domain.AgentAccountProblemRateLimited
	}
	return transient(r.identityProblem) || transient(r.limitsProblem)
}

// httpStatus429 matches an HTTP 429 named as a status, not any "429" digits
// (a port such as 127.0.0.1:42901 is not a rate limit).
var httpStatus429 = regexp.MustCompile(`\b(?:status|http|code)\W{0,3}429\b`)

// problemKind sorts a failed check by what failed, not by the error's text.
func problemKind(err error) domain.AgentAccountProblemKind {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return domain.AgentAccountProblemSlow
	case errors.Is(err, ports.ErrAgentBinaryNotFound), errors.Is(err, exec.ErrNotFound):
		return domain.AgentAccountProblemCLIMissing
	}
	// Some CLI clients pass a timeout on as text only.
	text := strings.ToLower(err.Error())
	if strings.Contains(text, "deadline exceeded") || strings.Contains(text, "timed out") {
		return domain.AgentAccountProblemSlow
	}
	if strings.Contains(text, "rate limit") || strings.Contains(text, "rate_limit") || strings.Contains(text, "too many requests") || httpStatus429.MatchString(text) {
		return domain.AgentAccountProblemRateLimited
	}
	return domain.AgentAccountProblemUnavailable
}

// knownUnusable reports a reading that shows the folder cannot launch as its
// account: logged in by no method at all, or signed in as another login. An
// API key is not a subscription, but the CLI launches with it, so it passes. A
// failed probe shows nothing, and no reading yet is not known.
func (r *reading) knownUnusable() bool {
	return r != nil && (r.mismatch || (r.identityErr == "" && !r.identity.LoggedIn))
}

// New builds the service. root must be absolute; the account folders' paths
// are stored exactly, because Claude keys each folder's login on the path text.
func New(store ports.AgentAccountStore, providers []ports.AgentAccountProviderCLI, root string, terminals Terminals, log *slog.Logger) *Service {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	byProvider := map[domain.AgentAccountProvider]ports.AgentAccountProviderCLI{}
	for _, p := range providers {
		byProvider[p.Provider()] = p
	}
	return &Service{
		defaultHarness: domain.HarnessClaudeCode, store: store, providers: byProvider, root: filepath.Clean(root), terminals: terminals, log: log,
		now: func() time.Time { return time.Now().UTC() }, readings: map[string]*reading{}, warnings: map[string]string{}, activeLogins: map[string]bool{}, retries: map[string]lifecycleRetry{},
		lastOrg: map[string]seenOrg{},
	}
}

// SetTerminals late-binds the terminal sign-ins run in, which the daemon builds
// after Session Manager.
func (s *Service) SetTerminals(terminals Terminals) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.terminals = terminals
}

// SetAgentStopper late-binds what stops the agents on an account before it is
// removed, so none keeps running on a login AO deletes.
func (s *Service) SetAgentStopper(stop func(ctx context.Context, accountID string) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopAgents = stop
}

// SetLimitLog late-binds where each limits read is kept.
func (s *Service) SetLimitLog(log ports.AgentLimitReadingStore) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.limitLog = log
}

// KeepReadingLimits reads each account's limits again once its last reading is stale, so the window history keeps going while no screen asks for limits.
func (s *Service) KeepReadingLimits(ctx context.Context, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !s.keepingLimits(ctx) {
				continue
			}
			if _, err := s.List(ctx, false); err != nil && ctx.Err() == nil {
				s.log.DebugContext(ctx, "background limits read failed", "error", err)
			}
		}
	}
}

func (s *Service) keepingLimits(ctx context.Context) bool {
	s.mu.Lock()
	log := s.limitLog
	s.mu.Unlock()
	return log != nil && log.LimitHistoryEnabled(ctx)
}

func (s *Service) recordLimits(ctx context.Context, account domain.AgentAccount, email, plan string, limits domain.AgentAccountLimits) {
	if len(limits.Windows) == 0 || !s.keepingLimits(ctx) {
		return
	}
	s.mu.Lock()
	log := s.limitLog
	s.mu.Unlock()
	at := limits.FetchedAt
	if at.IsZero() {
		at = s.now()
	}
	readings := make([]domain.AgentLimitReading, 0, len(limits.Windows))
	for _, w := range limits.Windows {
		readings = append(readings, domain.AgentLimitReading{
			AccountID: account.ID, Provider: account.Provider, Email: email, Plan: plan, WindowID: w.ID, Label: w.Label,
			DurationMinutes: w.DurationMinutes, UsedPercent: w.UsedPercent, ResetsAt: w.ResetsAt, ReadAt: at,
		})
	}
	if err := log.RecordAgentLimitReadings(context.WithoutCancel(ctx), readings); err != nil {
		s.log.WarnContext(ctx, "failed to record limit readings", "account", account.ID, "error", err)
	}
}

// AccountView is an account with its latest readings, as the app shows it.
type AccountView struct {
	IdentityCheckedAt *time.Time
	Account           domain.AgentAccount
	Default           bool
	SignedIn          bool
	// LoggedIn is any login the CLI launches with as this account, an API key
	// included; a folder signed in as another login is not.
	LoggedIn        bool
	IdentityErr     string
	IdentityProblem domain.AgentAccountProblemKind
	Limits          *domain.AgentAccountLimits
	LimitsErr       string
	LimitsProblem   domain.AgentAccountProblemKind
	SameLoginAs     string
	OrgID           string
	OrgName         string
}

type seenOrg struct{ id, name string }

// List returns every account, the computer's own first per provider. refresh
// reads identity and limits again even when the last reading is fresh.
func (s *Service) List(ctx context.Context, refresh bool) ([]AccountView, error) {
	accounts, err := s.accounts(ctx)
	if err != nil {
		return nil, err
	}
	var wg sync.WaitGroup
	for _, account := range accounts {
		if account.RetiredAt == nil && account.DisabledAt == nil && s.needsReading(account.ID, refresh) {
			wg.Go(func() { s.read(ctx, account) })
		}
	}
	wg.Wait()
	views := make([]AccountView, 0, len(accounts))
	for _, account := range accounts {
		if account.RetiredAt != nil {
			continue
		}
		def, _ := s.defaultID(ctx, account.Provider)
		s.mu.Lock()
		r := s.readings[account.ID]
		lastOrg := s.lastOrg[account.ID]
		s.mu.Unlock()
		view := AccountView{Account: account, Default: def == account.ID, OrgID: lastOrg.id, OrgName: lastOrg.name}
		if r != nil && account.DisabledAt == nil {
			view.SignedIn, view.IdentityErr, view.Limits, view.LimitsErr = r.identity.SignedIn, r.identityErr, r.limits, r.limitsErr
			view.IdentityProblem, view.LimitsProblem = r.identityProblem, r.limitsProblem
			view.LoggedIn = r.identity.LoggedIn && !r.mismatch
			if r.identity.SignedIn && r.identity.OrgID != "" {
				view.OrgID, view.OrgName = r.identity.OrgID, r.identity.OrgName
			}
			if r.identityErr == "" {
				checkedAt := r.at
				view.IdentityCheckedAt = &checkedAt
			}
			if account.Device() {
				view.Account.Email, view.Account.Plan = r.identity.Email, r.identity.Plan
			} else if r.identity.Plan != "" {
				view.Account.Plan = r.identity.Plan
			}
		}
		views = append(views, view)
	}
	markSameLogin(views)
	return views, nil
}

func markSameLogin(views []AccountView) {
	for i := range views {
		device := &views[i]
		if !device.Account.Device() || !device.SignedIn || device.Account.Email == "" {
			continue
		}
		for _, saved := range views {
			if !saved.Account.Device() && saved.Account.Provider == device.Account.Provider && saved.SignedIn && strings.EqualFold(saved.Account.Email, device.Account.Email) && sameOrg(saved.OrgID, device.OrgID) {
				device.SameLoginAs = saved.Account.ID
				break
			}
		}
	}
}

func sameOrg(a, b string) bool {
	return a == "" || b == "" || a == b
}

func (s *Service) needsReading(id string, refresh bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.readings[id]
	if refresh || r == nil {
		return true
	}
	ttl := freshFor
	switch {
	case r.failedOnItsOwn():
		// Before the empty-windows case: a failed read can keep earlier limits with no windows.
		ttl = min(freshFor, retryFailedAfter<<min(max(r.failures-1, 0), 8))
	case r.limits != nil && len(r.limits.Windows) == 0:
		// A plan that never reports windows is then read on every poll, one CLI call a minute.
		ttl = retryEmptyAfter
	}
	return s.now().Sub(r.at) > ttl
}

// read asks the provider's CLI for the account's identity and limits.
func (s *Service) read(ctx context.Context, account domain.AgentAccount) *reading {
	provider := s.providers[account.Provider]
	if provider == nil {
		return nil
	}
	caller := ctx
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	s.mu.Lock()
	s.readSeq++
	r := &reading{at: s.now(), seq: s.readSeq}
	s.mu.Unlock()
	var identity domain.AgentAccountIdentityCheck
	release, err := s.acquireProfileRead(ctx, account.ID)
	if err == nil {
		defer release()
		account, err = s.find(ctx, account.ID)
	}
	if err == nil {
		err = accountAvailability(account)
	}
	if err == nil {
		err = s.checkAccountProfile(account.Home)
	}
	if err == nil && account.ProfileKind == domain.AgentAccountProfileContained {
		err = s.prepare(provider, account.Home, account.ProfileKind)
	}
	if err == nil {
		identity, err = provider.Identity(ctx, account.Home)
	}
	if err == nil && identity.LoggedIn {
		err = s.checkCredentialStore(account.Provider, account.Home)
		if err != nil {
			identity = domain.AgentAccountIdentityCheck{}
		}
	}
	r.identity = identity
	if err != nil {
		r.identityErr = safeAccountProviderError(err).Error()
		s.log.Debug("account identity check failed", "provider", account.Provider, "reasonCode", r.identityErr)
		r.identityProblem = problemKind(err)
		// A failed check (a timeout under load) says nothing about the login:
		// keep what the last good one found, so it does not read as signed out.
		s.mu.Lock()
		if prev := s.readings[account.ID]; prev != nil && prev.identityErr == "" && !errors.Is(err, ports.ErrAgentAccountStorageUnsafe) && !errors.Is(err, ports.ErrAgentAccountStorageUnavailable) && !errors.Is(err, ports.ErrAgentAccountCredentialStoreUnsupported) {
			r.identity, r.limits = prev.identity, prev.limits
		}
		s.mu.Unlock()
	}
	s.mu.Lock()
	known := s.lastOrg[account.ID]
	s.mu.Unlock()
	switch {
	case err != nil:
	case !identity.SignedIn:
		r.limitsErr = "signed out"
	case !account.Device() && account.Email != "" && !strings.EqualFold(identity.Email, account.Email):
		// The right-account check: the folder must still hold the login it was added with.
		r.identity.SignedIn = false
		r.mismatch = true
		r.identityProblem = domain.AgentAccountProblemOtherLogin
		r.identityErr = "account_identity_mismatch"
	case !account.Device() && !sameOrg(known.id, identity.OrgID):
		r.identity.SignedIn = false
		r.mismatch = true
		r.identityProblem = domain.AgentAccountProblemOtherOrg
		r.identityErr = "account_organization_mismatch"
	default:
		if limits, limitsErr := provider.Limits(ctx, account.Home); limitsErr != nil {
			r.limitsErr = safeAccountProviderError(limitsErr).Error()
			r.limitsProblem = problemKind(limitsErr)
			s.mu.Lock()
			if prev := s.readings[account.ID]; prev != nil && prev.limits != nil && strings.EqualFold(prev.identity.Email, identity.Email) {
				r.limits = prev.limits
			}
			s.mu.Unlock()
		} else {
			r.limits = &limits
			email, plan := identity.Email, identity.Plan
			if email == "" {
				email = account.Email
			}
			if plan == "" {
				plan = account.Plan
			}
			s.recordLimits(ctx, account, email, plan, limits)
		}
	}
	if err == nil {
		confirmed := r.identity
		r.confirmed, r.confirmedSeq = &confirmed, r.seq
	}
	// A caller that went away (a closed request, a cancelled move) says nothing
	// about the account; caching its failure would refuse the account for the
	// rest of the cache window.
	if caller.Err() != nil {
		return r
	}
	s.mu.Lock()
	if prev := s.readings[account.ID]; prev != nil {
		if prev.confirmedSeq > r.seq {
			s.mu.Unlock()
			return prev
		}
		if err != nil {
			r.confirmed, r.confirmedSeq = prev.confirmed, prev.confirmedSeq
		}
	}
	if r.failedOnItsOwn() {
		r.failures = 1
		if prev := s.readings[account.ID]; prev != nil && prev.failedOnItsOwn() {
			r.failures = prev.failures + 1
		}
	}
	s.readings[account.ID] = r
	if err == nil && r.identity.SignedIn && !r.mismatch && r.identity.OrgID != "" && !account.Device() {
		s.lastOrg[account.ID] = seenOrg{id: r.identity.OrgID, name: r.identity.OrgName}
	}
	confirmed := s.loginConfirmed
	s.mu.Unlock()
	// The limits come from the provider's API under this login, so a reading
	// that has them shows the login worked when the reading began.
	if confirmed != nil && err == nil && r.identity.SignedIn && r.limits != nil && r.limitsErr == "" {
		confirmed(account, r.at)
	}
	return r
}

// ConfirmedLogins is, per account, the identity its last successful check
// found. A failed check keeps the one before it.
func (s *Service) ConfirmedLogins() map[string]domain.AgentAccountIdentityCheck {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]domain.AgentAccountIdentityCheck, len(s.readings))
	for id, r := range s.readings {
		if r.confirmed != nil {
			out[id] = *r.confirmed
		}
	}
	return out
}

// accounts is the computer's own account per provider, then the saved ones.
func (s *Service) accounts(ctx context.Context) ([]domain.AgentAccount, error) {
	saved, err := s.store.ListAgentAccounts(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]domain.AgentAccount, 0, len(saved)+len(domain.AgentAccountProviders))
	for _, p := range domain.AgentAccountProviders {
		if s.providers[p] == nil {
			continue
		}
		out = append(out, domain.AgentAccount{ID: domain.DeviceAgentAccountID(p), Provider: p})
		for _, account := range saved {
			if account.Provider == p {
				out = append(out, account)
			}
		}
	}
	return out, nil
}

func (s *Service) find(ctx context.Context, id string) (domain.AgentAccount, error) {
	accounts, err := s.accounts(ctx)
	if err != nil {
		return domain.AgentAccount{}, err
	}
	for _, account := range accounts {
		if account.ID == id {
			return account, nil
		}
	}
	return domain.AgentAccount{}, ports.ErrAgentAccountUnknown
}

// defaultID is the provider's account for new sessions; the computer's own
// only when none was picked. Invalid configured defaults never select device.
func (s *Service) defaultID(ctx context.Context, p domain.AgentAccountProvider) (string, error) {
	id, set, err := s.store.GetAgentAccountDefault(ctx, p)
	if err != nil {
		return "", err
	}
	if !set {
		return domain.DeviceAgentAccountID(p), nil
	}
	if id == "" {
		return "", ports.ErrAgentAccountUnknown
	}
	return id, nil
}

// DefaultSignIn describes the configured default, including an unusable one.
// It must not delegate to device readiness when a saved choice cannot launch.
func (s *Service) DefaultSignIn(ctx context.Context, harness domain.AgentHarness) (identity domain.AgentAccountIdentityCheck, handled bool) {
	p, ok := domain.AgentAccountProviderFor(harness)
	if !ok || s.providers[p] == nil {
		return identity, false
	}
	id, err := s.defaultID(ctx, p)
	if err != nil {
		return identity, true
	}
	if id == domain.DeviceAgentAccountID(p) {
		return identity, false
	}
	account, err := s.find(ctx, id)
	if err != nil {
		return identity, true
	}
	identity.Email = account.Email
	if _, err := s.pick(ctx, harness, id); err != nil {
		return identity, true
	}
	if r := s.cachedReading(id); r != nil {
		identity = r.identity
	}
	if account.Email != "" {
		identity.Email = account.Email
	}
	return identity, true
}

// DefaultLaunchEnv gives discovery the account a new task would use, without binding a session.
func (s *Service) DefaultLaunchEnv(ctx context.Context, harness domain.AgentHarness) (map[string]string, error) {
	p, ok := domain.AgentAccountProviderFor(harness)
	if !ok || s.providers[p] == nil {
		return nil, nil
	}
	account, err := s.newSessionAccount(ctx, p)
	if err != nil {
		return nil, err
	}
	return s.providers[p].LaunchEnv(account.Home), nil
}

// SetDefaultChanged late-binds what must hear that the account new sessions of
// a provider start on, or its sign-in, may have changed (agent readiness).
func (s *Service) SetDefaultChanged(changed func(domain.AgentAccountProvider)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.defaultChanged = changed
}

// SetLoginConfirmed late-binds what must hear that a reading found an
// account's login working, with the time the reading began: chat sessions on
// it that were refused their credentials can stop asking for a sign-in, however
// the login came back (AO's sign-in, the CLI's own, or a renewed token).
func (s *Service) SetLoginConfirmed(confirmed func(domain.AgentAccount, time.Time)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loginConfirmed = confirmed
}

func (s *Service) notifyDefaultChanged(p domain.AgentAccountProvider) {
	s.mu.Lock()
	changed := s.defaultChanged
	s.mu.Unlock()
	if changed != nil {
		changed(p)
	}
}

// RunsOnComputerLogin reports whether a new session of harness asking for
// accountID ("" for the default) would launch on the computer's own login.
// Harnesses without accounts always do.
func (s *Service) RunsOnComputerLogin(ctx context.Context, harness domain.AgentHarness, accountID string) bool {
	p, ok := domain.AgentAccountProviderFor(harness)
	if !ok || s.providers[p] == nil {
		return true
	}
	if accountID == "" {
		account, err := s.newSessionAccount(ctx, p)
		if err != nil {
			return false
		}
		accountID = account.ID
	}
	return accountID == domain.DeviceAgentAccountID(p)
}

// newSessionAccount reports only the configured default. An unavailable default
// is an explicit choice that must be repaired; it never selects another login.
func (s *Service) newSessionAccount(ctx context.Context, p domain.AgentAccountProvider) (domain.AgentAccount, error) {
	id, err := s.defaultID(ctx, p)
	if err != nil {
		return domain.AgentAccount{}, err
	}
	a, err := s.find(ctx, id)
	if err != nil {
		return domain.AgentAccount{}, err
	}
	if err := accountAvailability(a); err != nil {
		return domain.AgentAccount{}, err
	}
	if r := s.cachedReading(a.ID); r.knownUnusable() && s.now().Sub(r.at) <= freshFor {
		return domain.AgentAccount{}, ports.ErrAgentAccountSignedOut
	}
	return a, nil
}

func (s *Service) cachedReading(id string) *reading {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readings[id]
}

// RecheckSessionAccount reads the account a session runs on again, after its
// agent was refused for its credentials. Claude clears a folder's login when
// its refresh token is refused, so the account reads as signed out at once and
// new sessions stop starting on it, instead of after the cache window.
func (s *Service) RecheckSessionAccount(ctx context.Context, id domain.SessionID, harness domain.AgentHarness) {
	p, ok := domain.AgentAccountProviderFor(harness)
	if !ok || s.providers[p] == nil {
		return
	}
	account, _, err := s.SessionAccount(ctx, id, p)
	if err != nil || accountAvailability(account) != nil {
		s.log.Debug("recheck session account", "session", id, "error", err)
		return
	}
	s.read(ctx, account)
}

// SetDefault makes accountID the account new sessions of its provider start on.
// Running sessions and the computer's own CLI are not touched.
func (s *Service) SetDefault(ctx context.Context, accountID string) error {
	account, err := s.find(ctx, accountID)
	if err != nil {
		return err
	}
	if err := accountAvailability(account); err != nil {
		return err
	}
	if err := s.store.SetAgentAccountDefault(ctx, account.Provider, account.ID); err != nil {
		return err
	}
	s.notifyDefaultChanged(account.Provider)
	return nil
}

// LoginStart is a sign-in running in an embedded terminal.
type LoginStart struct {
	OperationID string
	Provider    domain.AgentAccountProvider
	Terminal    shellterm.ShellTerminal
}

// StartLogin creates a new account folder and runs the provider's own sign-in
// for it in a terminal.
func (s *Service) StartLogin(ctx context.Context, p domain.AgentAccountProvider) (LoginStart, error) {
	return s.startLogin(ctx, p, nil)
}

// StartSignInAgain runs the provider's sign-in in a saved account's own folder,
// so a signed-out account comes back with its sessions, history and settings.
func (s *Service) StartSignInAgain(ctx context.Context, accountID string) (LoginStart, error) {
	account, err := s.find(ctx, accountID)
	if err != nil {
		return LoginStart{}, err
	}
	if account.Device() {
		return LoginStart{}, ports.ErrAgentAccountDeviceFixed
	}
	return s.startLogin(ctx, account.Provider, &account)
}

// LoginStatus is how a sign-in stands.
type LoginStatus string

// Sign-in states.
const (
	LoginWaiting   LoginStatus = "waiting"
	LoginComplete  LoginStatus = "complete"
	LoginDuplicate LoginStatus = "duplicate"
	// LoginMismatch is a sign-in again with another login than the account's.
	LoginMismatch        LoginStatus = "mismatch"
	LoginNotSubscription LoginStatus = "not_subscription"
)

// ConsumeResetCredit spends one Codex usage-limit reset of the account.
func (s *Service) ConsumeResetCredit(ctx context.Context, accountID, idempotencyKey string) (domain.CodexResetCreditOutcome, error) {
	release, err := s.acquireProfileRead(ctx, accountID)
	if err != nil {
		return "", err
	}
	defer release()
	account, err := s.find(ctx, accountID)
	if err != nil {
		return "", err
	}
	if err := accountAvailability(account); err != nil {
		return "", err
	}
	crediter, ok := s.providers[account.Provider].(resetCrediter)
	if !ok {
		return "", ports.ErrAgentAccountProviderMismatch
	}
	if err := s.checkAccountProfile(account.Home); err != nil {
		return "", err
	}
	outcome, err := crediter.ConsumeResetCredit(ctx, account.Home, idempotencyKey)
	if err != nil {
		safe := safeAccountProviderError(err)
		s.log.Debug("account reset-credit failed", "provider", account.Provider, "reasonCode", safe.Error(), "errorType", fmt.Sprintf("%T", err))
		return "", safe
	}
	s.read(ctx, account)
	return outcome, nil
}

// BindSession pins a session to accountID for its harness's provider, or to the
// provider's default when accountID is empty. Other harnesses have no accounts.
func (s *Service) BindSession(ctx context.Context, id domain.SessionID, harness domain.AgentHarness, accountID string) error {
	selected, err := s.ResolveAccount(ctx, ports.AgentAccountResolutionRequest{Harness: harness, AccountID: accountID})
	if err != nil {
		return err
	}
	return s.BindResolvedSession(ctx, id, harness, selected)
}

// CommitSession records that the session now uses accountID, once its agent
// already runs there. The account was validated before the move; probing its
// CLI again here could only turn a live agent into a rollback.
func (s *Service) CommitSession(ctx context.Context, id domain.SessionID, harness domain.AgentHarness, accountID string) error {
	p, ok := domain.AgentAccountProviderFor(harness)
	if !ok || s.providers[p] == nil {
		return ports.ErrAgentAccountProviderMismatch
	}
	account, err := s.find(ctx, accountID)
	if err != nil {
		return err
	}
	if err := accountAvailability(account); err != nil {
		return err
	}
	if account.Provider != p {
		return ports.ErrAgentAccountProviderMismatch
	}
	return s.store.BindSessionAgentAccountWithSource(ctx, id, domain.ResolvedAgentAccount{AccountID: account.ID, Provider: p, Source: domain.AgentAccountSourceExplicit}, s.now())
}

// ValidateAccount checks that accountID can run harness now.
func (s *Service) ValidateAccount(ctx context.Context, harness domain.AgentHarness, accountID string) error {
	_, err := s.pick(ctx, harness, accountID)
	return err
}

// pick resolves an account chosen for harness. It must be of the harness's
// provider and signed in as itself, or the agent would start signed out or as
// someone else.
func (s *Service) pick(ctx context.Context, harness domain.AgentHarness, accountID string) (domain.AgentAccount, error) {
	release, err := s.acquireProfileRead(ctx, accountID)
	if err != nil {
		return domain.AgentAccount{}, err
	}
	defer release()
	p, ok := domain.AgentAccountProviderFor(harness)
	if !ok || s.providers[p] == nil {
		return domain.AgentAccount{}, ports.ErrAgentAccountProviderMismatch
	}
	account, err := s.find(ctx, accountID)
	if err != nil {
		return domain.AgentAccount{}, err
	}
	if err := accountAvailability(account); err != nil {
		return domain.AgentAccount{}, err
	}
	if account.Provider != p {
		return domain.AgentAccount{}, ports.ErrAgentAccountProviderMismatch
	}
	if err := s.checkAccountProfile(account.Home); err != nil {
		return domain.AgentAccount{}, err
	}
	s.mu.Lock()
	r := s.readings[account.ID]
	s.mu.Unlock()
	// A failed reading may be transient (a timeout under load); read again
	// rather than refuse the account on it for the rest of the cache window.
	if r == nil || r.identityErr != "" || s.now().Sub(r.at) > freshFor {
		s.read(ctx, account)
		s.mu.Lock()
		r = s.readings[account.ID]
		s.mu.Unlock()
	}
	if r == nil {
		return domain.AgentAccount{}, ports.ErrAgentAccountStorageUnavailable
	}
	if r.identityErr != "" && !r.mismatch {
		for _, category := range []error{ports.ErrAgentAccountCredentialStoreUnsupported, ports.ErrAgentAccountStorageUnsafe, ports.ErrAgentAccountStorageUnsupported} {
			if r.identityErr == category.Error() {
				return domain.AgentAccount{}, category
			}
		}
		return domain.AgentAccount{}, ports.ErrAgentAccountStorageUnavailable
	}
	if !r.identity.SignedIn && (!account.Device() || !r.identity.LoggedIn) {
		return domain.AgentAccount{}, ports.ErrAgentAccountSignedOut
	}
	if err := s.checkLaunchCredentialStore(ctx, account); err != nil {
		return domain.AgentAccount{}, err
	}
	if current := s.cachedReading(account.ID); current != nil && !current.identity.SignedIn && (!account.Device() || !current.identity.LoggedIn) {
		return domain.AgentAccount{}, ports.ErrAgentAccountSignedOut
	}
	if err := s.prepare(s.providers[p], account.Home, account.ProfileKind); err != nil {
		return domain.AgentAccount{}, err
	}
	return account, nil
}

// Existing non-Linux profiles keep their legacy runtime behavior. New profiles
// are refused by ensureAccountProfile until native file-only storage is verified.
func (s *Service) checkAccountProfile(home string) error {
	if home == "" || runtime.GOOS != "linux" {
		return nil
	}
	return ensureAccountProfile(s.root, home)
}

// prepare refuses unsafe storage and unshared history before a launch.
func (s *Service) prepare(provider ports.AgentAccountProviderCLI, home string, kind domain.AgentAccountProfileKind) error {
	if home == "" {
		return nil
	}
	if err := s.checkAccountProfile(home); err != nil {
		return err
	}
	var err error
	if legacy, ok := provider.(interface{ PrepareLegacy(string) error }); ok && kind != domain.AgentAccountProfileContained {
		err = legacy.PrepareLegacy(home)
	} else {
		err = provider.Prepare(home)
	}
	if kind == domain.AgentAccountProfileContained && err != nil {
		switch {
		case errors.Is(err, ports.ErrAgentAccountStorageUnsafe):
			return ports.ErrAgentAccountStorageUnsafe
		case errors.Is(err, ports.ErrAgentAccountStorageUnsupported):
			return ports.ErrAgentAccountStorageUnsupported
		case errors.Is(err, ports.ErrAgentAccountUnshared):
			return ports.ErrAgentAccountUnshared
		default:
			return ports.ErrAgentAccountStorageUnavailable
		}
	}
	if errors.Is(err, ports.ErrAgentAccountUnshared) {
		return err
	}
	if err != nil {
		s.log.Warn("prepare account folder", "provider", provider.Provider(), "reasonCode", "account_prepare_failed")
	}
	return nil
}

// SessionAccountID is the account a session uses for harness's provider, or
// "" when the harness has no accounts.
func (s *Service) SessionAccountID(ctx context.Context, id domain.SessionID, harness domain.AgentHarness) (string, error) {
	p, ok := domain.AgentAccountProviderFor(harness)
	if !ok || s.providers[p] == nil {
		return "", nil
	}
	account, _, err := s.SessionAccount(ctx, id, p)
	return account.ID, err
}

// SessionLimits reads only the session's pinned account. A refresh bypasses the
// display cache; a binding or identity change never falls back to the default.
func (s *Service) SessionLimits(ctx context.Context, id domain.SessionID, harness domain.AgentHarness, refresh bool) (string, domain.AgentAccountLimits, error) {
	p, ok := domain.AgentAccountProviderFor(harness)
	if !ok || s.providers[p] == nil {
		return "", domain.AgentAccountLimits{}, ports.ErrAgentAccountProviderMismatch
	}
	account, err := s.boundAccount(ctx, id, p)
	if err != nil {
		return "", domain.AgentAccountLimits{}, err
	}
	s.mu.Lock()
	r := s.readings[account.ID]
	s.mu.Unlock()
	if refresh || r == nil || s.now().Sub(r.at) > freshFor {
		r = s.read(ctx, account)
	}
	if err := ctx.Err(); err != nil {
		return account.ID, domain.AgentAccountLimits{}, err
	}
	current, err := s.boundAccount(ctx, id, p)
	if err != nil {
		return account.ID, domain.AgentAccountLimits{}, err
	}
	if current.ID != account.ID || current.Home != account.Home {
		return account.ID, domain.AgentAccountLimits{}, ports.ErrAgentAccountChanged
	}
	if r != nil && r.mismatch {
		return account.ID, domain.AgentAccountLimits{}, ports.ErrAgentAccountChanged
	}
	if r == nil {
		return account.ID, domain.AgentAccountLimits{}, errors.New("account limits are unavailable")
	}
	if r.identityErr != "" {
		return account.ID, domain.AgentAccountLimits{}, fmt.Errorf("read account identity: %s", r.identityErr)
	}
	if !r.identity.SignedIn {
		return account.ID, domain.AgentAccountLimits{}, ports.ErrAgentAccountSignedOut
	}
	if r.limitsErr != "" {
		return account.ID, domain.AgentAccountLimits{}, fmt.Errorf("read account limits: %s", r.limitsErr)
	}
	if r.limits == nil {
		return account.ID, domain.AgentAccountLimits{}, errors.New("account limits are unavailable")
	}
	return account.ID, *r.limits, nil
}

// HandoffTarget picks the account with the most verified room for a usage-limited session, or "" when none has room.
func (s *Service) HandoffTarget(ctx context.Context, id domain.SessionID, harness domain.AgentHarness, model string) (string, error) {
	p, ok := domain.AgentAccountProviderFor(harness)
	if !ok || s.providers[p] == nil {
		return "", ports.ErrAgentAccountProviderMismatch
	}
	current, err := s.boundAccount(ctx, id, p)
	if err != nil {
		return "", err
	}
	accounts, err := s.accounts(ctx)
	if err != nil {
		return "", err
	}
	headroom := make([]float64, len(accounts))
	var wg sync.WaitGroup
	for i, account := range accounts {
		if account.Provider != p || account.ID == current.ID || accountAvailability(account) != nil {
			continue
		}
		wg.Go(func() {
			if r := s.read(ctx, account); r != nil && r.identityErr == "" && r.identity.SignedIn && r.limitsErr == "" && r.limits != nil {
				headroom[i], _ = r.limits.HandoffHeadroom(p, model)
			}
		})
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if after, err := s.boundAccount(ctx, id, p); err != nil {
		return "", err
	} else if after.ID != current.ID {
		return "", ports.ErrAgentAccountChanged
	}
	best := -1
	for i, room := range headroom {
		if room > 0 && (best < 0 || room > headroom[best]) {
			best = i
		}
	}
	if best < 0 {
		s.log.Info("account handoff: no target", "session", id, "model", model)
		return "", nil
	}
	s.log.Info("account handoff: target", "session", id, "model", model, "target", accounts[best].ID)
	return accounts[best].ID, nil
}

func (s *Service) boundAccount(ctx context.Context, id domain.SessionID, p domain.AgentAccountProvider) (domain.AgentAccount, error) {
	accountID, bound, err := s.store.GetSessionAgentAccount(ctx, id, p)
	if err != nil {
		return domain.AgentAccount{}, err
	}
	if !bound {
		return domain.AgentAccount{}, ports.ErrAgentAccountChanged
	}
	account, err := s.find(ctx, accountID)
	if errors.Is(err, ports.ErrAgentAccountUnknown) || (err == nil && account.Provider != p) {
		return domain.AgentAccount{}, ports.ErrAgentAccountChanged
	}
	if err == nil && accountAvailability(account) != nil {
		return domain.AgentAccount{}, ports.ErrAgentAccountChanged
	}
	return account, err
}

// SessionAccount is the account a session uses for the provider now: its
// binding, or the provider's default only when it has no binding. Legacy
// dangling bindings project as retired metadata: their original ID survives,
// without inventing a home or silently selecting the computer's login.
func (s *Service) SessionAccount(ctx context.Context, id domain.SessionID, p domain.AgentAccountProvider) (domain.AgentAccount, bool, error) {
	accountID, bound, err := s.store.GetSessionAgentAccount(ctx, id, p)
	if err != nil {
		return domain.AgentAccount{}, false, err
	}
	if bound {
		account, err := s.find(ctx, accountID)
		if errors.Is(err, ports.ErrAgentAccountUnknown) {
			retired := time.Unix(0, 0).UTC()
			return domain.AgentAccount{ID: accountID, Provider: p, RetiredAt: &retired}, true, nil
		}
		return account, true, err
	}
	def, err := s.defaultID(ctx, p)
	if err != nil {
		return domain.AgentAccount{}, false, err
	}
	account, err := s.find(ctx, def)
	if errors.Is(err, ports.ErrAgentAccountUnknown) {
		retired := time.Unix(0, 0).UTC()
		return domain.AgentAccount{ID: def, Provider: p, RetiredAt: &retired}, false, nil
	}
	return account, false, err
}

// SessionAccounts is the account a session uses for each provider.
func (s *Service) SessionAccounts(ctx context.Context, id domain.SessionID) (map[domain.AgentAccountProvider]string, error) {
	out := map[domain.AgentAccountProvider]string{}
	for _, p := range domain.AgentAccountProviders {
		if s.providers[p] == nil {
			continue
		}
		account, _, err := s.SessionAccount(ctx, id, p)
		if err != nil {
			return nil, err
		}
		out[p] = account.ID
	}
	return out, nil
}

// SessionLaunchEnv is the environment that runs each provider's CLI under the
// session's account. A session with no account for a provider yet is bound to
// that provider's default here, so a later default change does not move it.
func (s *Service) SessionLaunchEnv(ctx context.Context, id domain.SessionID, harness domain.AgentHarness) (map[string]string, error) {
	return s.launchEnv(ctx, id, domain.AgentAccount{}, harness, "")
}

// SessionLaunchEnvWith is SessionLaunchEnv with accountID in place of the
// session's account for its provider, for starting an agent on an account the
// session moves to. The session's binding is not changed.
func (s *Service) SessionLaunchEnvWith(ctx context.Context, id domain.SessionID, accountID string, harness domain.AgentHarness) (map[string]string, error) {
	account, err := s.find(ctx, accountID)
	if err != nil {
		return nil, err
	}
	p, ok := domain.AgentAccountProviderFor(harness)
	if !ok || p != account.Provider {
		return nil, ports.ErrAgentAccountProviderMismatch
	}
	return s.launchEnv(ctx, id, account, harness, "")
}

func (s *Service) launchEnv(ctx context.Context, id domain.SessionID, override domain.AgentAccount, harness domain.AgentHarness, reviewID string) (map[string]string, error) {
	env := map[string]string{}
	releases := make([]func(), 0, len(domain.AgentAccountProviders))
	defer func() {
		for _, release := range releases {
			release()
		}
	}()
	var errs []error
	active, _ := domain.AgentAccountProviderFor(harness)
	validationError := func(p domain.AgentAccountProvider, id string, err error) {
		occupied := errors.Is(err, ports.ErrAgentAccountUnavailableHomeOccupied)
		if p == active || occupied {
			if err != nil {
				errs = append(errs, err)
			}
			if !occupied {
				return
			}
		}
		reason := ""
		if err != nil {
			reason = safeAccountProviderError(err).Error()
		}
		s.mu.Lock()
		previous := s.warnings[id]
		if reason == "" {
			delete(s.warnings, id)
		} else {
			s.warnings[id] = reason
		}
		s.mu.Unlock()
		if reason != "" && previous != reason {
			message := "non-launch account unavailable"
			if occupied {
				message = ports.AgentAccountUnavailableHomeOccupiedMessage
			}
			s.log.Warn(message, "provider", p, "accountId", id, "reasonCode", reason)
		}
	}
	for _, p := range domain.AgentAccountProviders {
		provider := s.providers[p]
		if provider == nil {
			continue
		}
		account := override
		if override.Provider != p {
			var bound bool
			var err error
			if reviewID == "" {
				account, bound, err = s.SessionAccount(ctx, id, p)
			} else {
				account, bound, err = s.reviewerAccount(ctx, reviewID, p)
			}

			if err != nil {
				// Resolution failures leave no known home to pin. Unlike the
				// provider-scoped validation below, they cannot be ignored safely.
				errs = append(errs, err)
				continue
			}
			if !bound {
				if err := s.bindExecutionAccount(ctx, id, reviewID, p, account); err != nil {
					errs = append(errs, err)
				}
				if reviewID != "" {
					account, _, err = s.reviewerAccount(ctx, reviewID, p)
					if err != nil {
						errs = append(errs, err)
						continue
					}
				}
			}
		}
		if account.Device() {
			continue
		}
		release, refreshErr := s.acquireProfileRead(ctx, account.ID)
		if refreshErr == nil {
			releases = append(releases, release)
			current, err := s.find(ctx, account.ID)
			if err == nil {
				account = current
			} else if !errors.Is(err, ports.ErrAgentAccountUnknown) || account.RetiredAt == nil || account.Home != "" {
				refreshErr = err
			}
			// Pre-tombstone deleted rows deliberately remain synthetic retired
			// owners. Every branch below still pins their unavailable namespace.
		}
		// Even a rejected environment stays isolated from the device login.
		// Legacy deleted bindings have no known home: use a fixed reserved
		// namespace, never an empty home or a path derived from the missing ID.
		// Availability is checked before Prepare, so this home is not created.
		home := account.Home
		var namespaceErr error
		if home == "" {
			reserved := filepath.Join(s.root, ".unavailable")
			home = filepath.Join(reserved, string(p))
			// Native children may create these directories. Only an empty,
			// owned reserved skeleton can be removed; all data is preserved.
			s.unavailableMu.Lock()
			recovered, err := recoverUnavailableNamespace(s.root)
			s.unavailableMu.Unlock()
			namespaceErr = err
			if recovered {
				s.log.Info("removed empty reserved unavailable account folders", "provider", p, "reasonCode", "account_unavailable_home_recovered")
			}
		}
		for key, value := range provider.LaunchEnv(home) {
			env[key] = value
		}
		if namespaceErr != nil {
			validationError(p, account.ID, namespaceErr)
			if p != active && !errors.Is(namespaceErr, ports.ErrAgentAccountUnavailableHomeOccupied) {
				// An occupied or unsafe reserved namespace is not a known private
				// home. Refuse the whole launch, including non-harness children.
				errs = append(errs, namespaceErr)
			}
			continue
		}
		if refreshErr != nil {
			validationError(p, account.ID, refreshErr)
			continue
		}
		if err := accountAvailability(account); err != nil {
			validationError(p, account.ID, err)
			continue
		}
		// Revalidate before producing the launch environment, even when the
		// account identity is still cached.
		if err := s.prepare(provider, account.Home, account.ProfileKind); err != nil {
			validationError(p, account.ID, err)
			continue
		}
		if p == active {
			validationError(p, account.ID, s.checkLaunchCredentialStore(ctx, account))
		} else {
			validationError(p, account.ID, s.checkCredentialStore(account.Provider, account.Home))
		}
	}
	return env, errors.Join(errs...)
}

// An absent file can also mean a normal logout. Refresh the native reading
// before calling that an unsupported store; never probe an existing unsafe
// credential entry. This reads only metadata, not credential bytes.
func (s *Service) checkLaunchCredentialStore(ctx context.Context, account domain.AgentAccount) error {
	err := s.checkCredentialStore(account.Provider, account.Home)
	if !errors.Is(err, ports.ErrAgentAccountCredentialStoreUnsupported) {
		return err
	}
	if _, statErr := os.Lstat(filepath.Join(account.Home, "auth.json")); !errors.Is(statErr, os.ErrNotExist) {
		return err
	}
	r := s.read(ctx, account)
	if ctx.Err() != nil || r != nil && (r.identityErr == context.DeadlineExceeded.Error() || r.identityErr == context.Canceled.Error()) {
		return ports.ErrAgentAccountStorageUnavailable
	}
	if r.knownUnusable() {
		return ports.ErrAgentAccountSignedOut
	}
	if r == nil {
		return ports.ErrAgentAccountStorageUnavailable
	}
	if r.identityErr != "" && r.identityErr != ports.ErrAgentAccountCredentialStoreUnsupported.Error() {
		for _, category := range []error{ports.ErrAgentAccountStorageUnsafe, ports.ErrAgentAccountStorageUnsupported} {
			if r.identityErr == category.Error() {
				return category
			}
		}
		return ports.ErrAgentAccountStorageUnavailable
	}
	if r.identityErr == "" {
		return s.checkCredentialStore(account.Provider, account.Home)
	}
	return err
}

// AccountUsage is one account's model usage in AO over a period.
type AccountUsage struct {
	AccountID    string
	Days         []DayUsage
	Models       []ModelUsage
	InputTokens  int64
	OutputTokens int64
	CostNanos    int64
}

// DayUsage is one UTC day of an account's usage.
type DayUsage struct {
	Day       string
	Tokens    int64
	CostNanos int64
}

// ModelUsage is one model's share of an account's usage.
type ModelUsage struct {
	Model     string
	Tokens    int64
	CostNanos int64
}

// Usage sums each account's usage in AO sessions over the last days, from AO's
// own reading of the session logs.
func (s *Service) Usage(ctx context.Context, days int) ([]AccountUsage, error) {
	if days <= 0 || days > 90 {
		days = 30
	}
	since := s.now().AddDate(0, 0, -days+1).Truncate(24 * time.Hour)
	rows, err := s.store.AgentAccountUsageSince(ctx, since)
	if err != nil {
		return nil, err
	}
	byAccount := map[string]*AccountUsage{}
	dayIndex := map[string]map[string]int{}
	modelIndex := map[string]map[string]int{}
	for _, row := range rows {
		u := byAccount[row.AccountID]
		if u == nil {
			u = &AccountUsage{AccountID: row.AccountID}
			byAccount[row.AccountID] = u
			dayIndex[row.AccountID], modelIndex[row.AccountID] = map[string]int{}, map[string]int{}
		}
		tokens := row.InputTokens + row.OutputTokens
		u.InputTokens += row.InputTokens
		u.OutputTokens += row.OutputTokens
		u.CostNanos += row.CostNanos
		if i, ok := dayIndex[row.AccountID][row.Day]; ok {
			u.Days[i].Tokens += tokens
			u.Days[i].CostNanos += row.CostNanos
		} else {
			dayIndex[row.AccountID][row.Day] = len(u.Days)
			u.Days = append(u.Days, DayUsage{Day: row.Day, Tokens: tokens, CostNanos: row.CostNanos})
		}
		if i, ok := modelIndex[row.AccountID][row.Model]; ok {
			u.Models[i].Tokens += tokens
			u.Models[i].CostNanos += row.CostNanos
		} else {
			modelIndex[row.AccountID][row.Model] = len(u.Models)
			u.Models = append(u.Models, ModelUsage{Model: row.Model, Tokens: tokens, CostNanos: row.CostNanos})
		}
	}
	out := make([]AccountUsage, 0, len(byAccount))
	for _, u := range byAccount {
		sort.Slice(u.Models, func(i, j int) bool { return u.Models[i].Tokens > u.Models[j].Tokens })
		out = append(out, *u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AccountID < out[j].AccountID })
	return out, nil
}

// File evidence is required only for Linux managed Codex profiles. The profile
// boundary verifies AO-root containment before this metadata-only check.
func (s *Service) checkCredentialStore(p domain.AgentAccountProvider, home string) error {
	if runtime.GOOS != "linux" || home == "" || p != domain.AgentAccountProviderCodex {
		return nil
	}
	if err := s.checkAccountProfile(home); err != nil {
		return err
	}
	return agentlaunch.ValidateCodexFileStore(home)
}

func accountAvailability(a domain.AgentAccount) error {
	if a.RetiredAt != nil {
		return ports.ErrAgentAccountUnknown
	}
	if a.DisabledAt != nil {
		return ports.ErrAgentAccountSignedOut
	}
	return nil
}
