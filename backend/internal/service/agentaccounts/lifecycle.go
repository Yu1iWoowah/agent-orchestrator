package agentaccounts

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/shellterm"
)

// OperationError exposes a non-secret progress reference with a canonical
// category. The provider's original error and cause are never retained.
type OperationError struct {
	OperationID string
	Reviewer    *ports.AgentAccountReviewerInUseError
	category    error
}

func (e *OperationError) Error() string { return e.category.Error() }
func (e *OperationError) Unwrap() error { return e.category }

func (s *Service) journal() (ports.AgentAccountJournal, error) {
	j, ok := s.store.(ports.AgentAccountJournal)
	if !ok {
		return nil, ports.ErrAgentAccountStorageUnavailable
	}
	return j, nil
}
func (s *Service) operation(ctx context.Context, id string) (domain.AgentAccountOperation, error) {
	j, e := s.journal()
	if e != nil {
		return domain.AgentAccountOperation{}, e
	}
	o, found, e := j.GetAgentAccountOperation(ctx, id)
	if e != nil {
		return o, ports.ErrAgentAccountStorageUnavailable
	}
	if !found {
		return o, ports.ErrAgentAccountLoginUnknown
	}
	return o, nil
}
func (s *Service) saveOperation(ctx context.Context, o *domain.AgentAccountOperation) error {
	prior, e := s.operation(ctx, o.ID)
	if e != nil {
		return e
	}
	if prior.Phase == o.Phase && prior.Outcome == o.Outcome && prior.ReasonCode == o.ReasonCode && prior.ResultAccountID == o.ResultAccountID {
		o.UpdatedAt = prior.UpdatedAt
		return nil // Do not emit CDC for an unchanged retry result.
	}
	o.UpdatedAt = s.now()
	j, e := s.journal()
	if e != nil {
		return e
	}
	if e = j.UpdateAgentAccountOperation(ctx, *o); e != nil {
		return ports.ErrAgentAccountStorageUnavailable
	}
	return nil
}
func (s *Service) activeOperation(ctx context.Context, id string) (domain.AgentAccountOperation, bool, error) {
	j, e := s.journal()
	if e != nil {
		return domain.AgentAccountOperation{}, false, e
	}
	rows, e := j.ListAgentAccountOperations(ctx)
	if e != nil {
		return domain.AgentAccountOperation{}, false, ports.ErrAgentAccountStorageUnavailable
	}
	for _, o := range rows {
		if o.AccountID == id && o.Phase != "complete" {
			return o, true, nil
		}
	}
	return domain.AgentAccountOperation{}, false, nil
}

func (s *Service) startLogin(ctx context.Context, p domain.AgentAccountProvider, account *domain.AgentAccount) (LoginStart, error) {
	if runtime.GOOS != "linux" {
		return LoginStart{}, ports.ErrAgentAccountStorageUnsupported
	}
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if account != nil {
		release, err := s.acquireProfileMutation(account.ID)
		if err != nil {
			return LoginStart{}, err
		}
		defer release()
		current, err := s.find(ctx, account.ID)
		if err != nil {
			return LoginStart{}, err
		}
		account = &current
		ctx = context.WithValue(ctx, profileMutationContextKey{}, account.ID)
		if err := s.refuseLiveOwners(ctx, account.ID); err != nil {
			return LoginStart{}, err
		}
	}
	provider := s.providers[p]
	s.mu.Lock()
	terminals := s.terminals
	s.mu.Unlock()
	if provider == nil || terminals == nil {
		return LoginStart{}, ports.ErrAgentAccountProviderMismatch
	}
	j, e := s.journal()
	if e != nil {
		return LoginStart{}, e
	}
	id := uuid.NewString()
	home := filepath.Join(s.root, string(p), id)
	kind := domain.AgentAccountProfileContained
	o := domain.AgentAccountOperation{ID: id, AccountID: id, Provider: p, Action: "login", Phase: "pending", TerminalID: "shellterm-" + id, CreatedAt: s.now(), UpdatedAt: s.now()}
	name := map[domain.AgentAccountProvider]string{domain.AgentAccountProviderClaude: "Claude", domain.AgentAccountProviderCodex: "Codex"}[p]
	title := "Add " + name + " account"
	if account != nil {
		if account.RetiredAt != nil {
			return LoginStart{}, ports.ErrAgentAccountUnknown
		}
		if _, busy, e := s.activeOperation(ctx, account.ID); e != nil {
			return LoginStart{}, e
		} else if busy {
			return LoginStart{}, ports.ErrAgentAccountOperationBusy
		}
		if e := s.checkAccountProfile(account.Home); e != nil {
			return LoginStart{}, e
		}
		home, kind = account.Home, account.ProfileKind
		o.AccountID = account.ID
		o.Action = "login-again"
		o.WasDisabled = account.DisabledAt != nil
		title = "Sign in to " + name + " account"
	}
	// Persist before even creating the pending directory. No credential, argv,
	// env, output or path is journaled.
	if e = j.CreateAgentAccountOperation(ctx, o); e != nil {
		return LoginStart{}, ports.ErrAgentAccountStorageUnavailable
	}
	fail := func(cause error) (LoginStart, error) {
		o.Outcome = "cancelled"
		o.Phase = "closing"
		if s.saveOperation(context.WithoutCancel(ctx), &o) == nil {
			if account != nil {
				_ = s.resumeOperationLocked(context.WithoutCancel(ctx), &o)
			} else {
				_ = s.resumeOperation(context.WithoutCancel(ctx), &o)
			}
		}
		return LoginStart{}, cause
	}
	if e = ensureAccountProfile(s.root, home); e != nil {
		return fail(e)
	}
	if e = s.prepare(provider, home, kind); e != nil {
		return fail(e)
	}
	argv, e := provider.LoginArgv(ctx)
	if e != nil {
		return fail(safeAccountProviderError(e))
	}
	s.activeLogins[id] = true
	terminal, e := terminals.OpenCommandTerminal(ctx, shellterm.OpenCommandTerminalInput{HandleID: o.TerminalID, Argv: argv, Env: provider.LaunchEnv(home), WorkingDir: home, Title: title})
	if e != nil {
		delete(s.activeLogins, id)
		return fail(ports.ErrAgentAccountStorageUnavailable)
	}
	return LoginStart{OperationID: id, Provider: p, Terminal: terminal}, nil
}

// VerifyLogin serializes native profile preparation/probes with cancellation.
// A cancelled operation must never be recreated by an in-flight native Prepare.
func (s *Service) VerifyLogin(ctx context.Context, id string) (LoginStatus, *AccountView, error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	return s.verifyLogin(ctx, id)
}

// verifyLogin is shared with recovery; callers hold opMu throughout native I/O.
func (s *Service) verifyLogin(ctx context.Context, id string) (LoginStatus, *AccountView, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	o, e := s.operation(ctx, id)
	if e != nil {
		return "", nil, e
	}
	if o.Action != "login" && o.Action != "login-again" {
		return "", nil, ports.ErrAgentAccountLoginUnknown
	}
	if o.Phase != "pending" {
		return s.loginResult(ctx, &o)
	}
	account, e := s.operationAccount(ctx, o)
	if e != nil {
		return "", nil, e
	}
	provider := s.providers[o.Provider]
	if e := s.prepare(provider, account.Home, account.ProfileKind); e != nil {
		return "", nil, e
	}
	identity, e := provider.Identity(ctx, account.Home)
	if e != nil {
		return "", nil, safeAccountProviderError(e)
	}
	if !identity.SignedIn {
		return LoginWaiting, nil, nil
	}
	if e := s.checkCredentialStore(o.Provider, account.Home); e != nil {
		return "", nil, e
	}
	// Refresh the catalog inside the same boundary as acceptance. Two
	// operations must not both commit the same identity from a stale list.
	views, e := s.List(ctx, false)
	if e != nil {
		return "", nil, e
	}
	o.Outcome = "complete"
	o.ResultAccountID = o.AccountID
	for _, v := range views {
		if v.Account.ID != o.AccountID && v.Account.Provider == o.Provider && (v.SignedIn || !v.Account.Device()) && v.Account.Email != "" && strings.EqualFold(v.Account.Email, identity.Email) {
			o.Outcome = "duplicate"
			o.ResultAccountID = v.Account.ID
			break
		}
	}
	if o.Action == "login-again" && account.Email != "" && !strings.EqualFold(account.Email, identity.Email) {
		o.Outcome = "mismatch"
		o.ResultAccountID = account.ID
	}
	s.mu.Lock()
	legacyCheck := s.legacyIdentityCheck
	s.mu.Unlock()
	if o.Outcome == "complete" && legacyCheck != nil {
		conflict, err := legacyCheck(ctx, o.Provider, identity.Email)
		if err != nil {
			return "", nil, ports.ErrAgentAccountStorageUnavailable
		}
		if conflict {
			o.Outcome = "legacy-conflict"
			o.ResultAccountID = ""
		}
	}
	if o.Outcome == "complete" {
		account.Email, account.Plan = identity.Email, identity.Plan
		o.Phase = "accepted"
		o.UpdatedAt = s.now()
		j, _ := s.journal()
		if e = j.CommitAgentAccountLogin(ctx, o, account); e != nil {
			return "", nil, ports.ErrAgentAccountStorageUnavailable
		}
	} else {
		o.Phase = "closing"
		if e := s.saveOperation(ctx, &o); e != nil {
			return "", nil, e
		}
	}
	return s.loginResult(ctx, &o)
}
func (s *Service) loginResult(ctx context.Context, o *domain.AgentAccountOperation) (LoginStatus, *AccountView, error) {
	if o.Phase != "complete" {
		if e := s.resumeOperation(ctx, o); e != nil {
			return "", nil, e
		}
	}
	if o.Outcome == "legacy-conflict" {
		return "", nil, &OperationError{OperationID: o.ID, category: ports.ErrAgentAccountLegacyIdentityConflict}
	}
	if o.Outcome == "cancelled" {
		return "", nil, ports.ErrAgentAccountLoginUnknown
	}
	status := LoginStatus(o.Outcome)
	if status != LoginComplete && status != LoginDuplicate && status != LoginMismatch {
		return "", nil, ports.ErrAgentAccountLoginUnknown
	}
	views, e := s.List(ctx, true)
	if e != nil {
		return "", nil, e
	}
	for _, v := range views {
		if v.Account.ID == o.ResultAccountID {
			return status, &v, nil
		}
	}
	return status, nil, nil
}

// CancelLogin records cancellation before closing the writer or cleaning a profile.
func (s *Service) CancelLogin(ctx context.Context, id string) error {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	return s.cancelLogin(ctx, id)
}

// cancelLogin is also used after recovery confirms an exited child is unsigned.
// Callers hold opMu.
func (s *Service) cancelLogin(ctx context.Context, id string) error {
	o, e := s.operation(ctx, id)
	if e != nil {
		return e
	}
	if o.Action != "login" && o.Action != "login-again" {
		return ports.ErrAgentAccountLoginUnknown
	}
	if o.Phase == "complete" {
		return nil
	}
	if o.Outcome == "" {
		o.Outcome = "cancelled"
		o.Phase = "closing"
		if e := s.saveOperation(ctx, &o); e != nil {
			return e
		}
	}
	return s.resumeOperation(ctx, &o)
}

func (s *Service) operationAccount(ctx context.Context, o domain.AgentAccountOperation) (domain.AgentAccount, error) {
	if o.Action == "login" {
		return domain.AgentAccount{ID: o.AccountID, Provider: o.Provider, Home: filepath.Join(s.root, string(o.Provider), o.AccountID), CreatedAt: o.CreatedAt, ProfileKind: domain.AgentAccountProfileContained}, nil
	}
	return s.find(ctx, o.AccountID)
}
func (s *Service) unconfirmed(ctx context.Context, o *domain.AgentAccountOperation, cause error) error {
	o.ReasonCode = "account_cleanup_unconfirmed"
	if errors.Is(cause, ports.ErrAgentAccountStorageUnsafe) {
		o.ReasonCode = "account_storage_unsafe"
	}
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if e := s.saveOperation(recordCtx, o); e != nil {
		return e
	}
	s.log.Warn("account lifecycle unconfirmed", "operationId", o.ID, "accountId", o.AccountID, "reasonCode", o.ReasonCode)
	return &OperationError{OperationID: o.ID, category: ports.ErrAgentAccountCleanupUnconfirmed}
}

type lifecycleRetry struct {
	failures int
	next     time.Time
}

func (s *Service) deferOperationRetry(id string) {
	retry := s.retries[id]
	if retry.failures < 5 {
		retry.failures++
	}
	delay := min(time.Minute<<(retry.failures-1), 15*time.Minute)
	retry.next = s.now().Add(delay)
	s.retries[id] = retry
}

func (s *Service) resumeOperation(ctx context.Context, o *domain.AgentAccountOperation) error {
	release, err := s.acquireProfileMutation(o.AccountID)
	if err != nil {
		return &OperationError{OperationID: o.ID, category: err}
	}
	defer release()
	return s.resumeOperationLocked(ctx, o)
}

// Callers hold both opMu and the exclusive account profile lock.
func (s *Service) resumeOperationLocked(ctx context.Context, o *domain.AgentAccountOperation) (err error) {
	ctx = context.WithValue(ctx, profileMutationContextKey{}, o.AccountID)
	defer func() {
		if err != nil {
			s.deferOperationRetry(o.ID)
		} else {
			delete(s.retries, o.ID)
		}
	}()
	if runtime.GOOS != "linux" {
		return ports.ErrAgentAccountStorageUnsupported
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	if o.Phase == "complete" {
		return nil
	}
	// Each completed phase is durable before the next side effect. Retrying
	// logout is allowed, but no uncertain result can enable/delete the account.
	account, e := s.operationAccount(ctx, *o)
	if e != nil {
		return e
	}
	if (o.Action == "logout" || o.Action == "remove") && o.Phase != "deleting" {
		s.mu.Lock()
		stop := s.stopAgents
		s.mu.Unlock()
		if stop != nil {
			if e = stop(ctx, o.AccountID); e != nil {
				o.ReasonCode = "account_operation_busy"
				if e := s.saveOperation(ctx, o); e != nil {
					return e
				}
				var reviewer *ports.AgentAccountReviewerInUseError
				errors.As(e, &reviewer)
				if reviewer != nil {
					if owners, err := s.UsageOwners(ctx, o.AccountID); err == nil {
						for _, owner := range owners {
							if owner.ReviewID == reviewer.ReviewID {
								copy := *reviewer
								copy.SessionName = owner.SessionName
								copy.PRURL = owner.PRURL
								reviewer = &copy
								break
							}
						}
					}
				}
				return &OperationError{OperationID: o.ID, Reviewer: reviewer, category: ports.ErrAgentAccountInUse}
			}
		}
	}

	if o.TerminalID != "" {
		s.mu.Lock()
		term := s.terminals
		s.mu.Unlock()
		if term == nil {
			return s.unconfirmed(ctx, o, ports.ErrAgentAccountStorageUnavailable)
		}
		e = term.CloseCommandTerminal(ctx, o.TerminalID)
		if e != nil {
			return s.unconfirmed(ctx, o, e)
		}
	}
	if o.Outcome == "complete" || o.Action == "login-again" && o.Outcome == "cancelled" {
		o.Phase = "complete"
		o.ReasonCode = ""
		if e := s.saveOperation(ctx, o); e != nil {
			return e
		}
		delete(s.activeLogins, o.ID)
		s.invalidateAccount(o.AccountID)
		s.notifyDefaultChanged(o.Provider)
		return nil
	}
	if o.Phase != "deleting" {
		if _, e = os.Lstat(account.Home); errors.Is(e, os.ErrNotExist) && o.Action == "login" {
			o.Phase = "complete"
			o.ReasonCode = ""
			delete(s.activeLogins, o.ID)
			return s.saveOperation(ctx, o)
		}
		if e = s.checkAccountProfile(account.Home); e != nil {
			return s.unconfirmed(ctx, o, e)
		}
		if o.Phase != "logging-out" {
			o.Phase = "logging-out"
			o.ReasonCode = ""
			if e := s.saveOperation(ctx, o); e != nil {
				return e
			}
		}
		provider := s.providers[o.Provider]
		if provider == nil {
			return s.unconfirmed(ctx, o, ports.ErrAgentAccountProviderMismatch)
		}
		// Native logout removes Codex auth.json. Re-probe before every retry,
		// including saved accounts, so absence after confirmed sign-out is
		// idempotent rather than an unsupported credential store assertion.
		identity, err := provider.Identity(ctx, account.Home)
		if err != nil {
			return s.unconfirmed(ctx, o, err)
		}
		if identity.LoggedIn {
			if e = provider.Logout(ctx, account.Home); e != nil {
				return s.unconfirmed(ctx, o, e)
			}
		}
		if o.Action == "remove" || o.Action == "login" {
			o.Phase = "deleting"
			if e := s.saveOperation(ctx, o); e != nil {
				return e
			}
		}
	}
	if o.Phase == "deleting" {
		if e = removeAccountProfile(s.root, account.Home); e != nil {
			return s.unconfirmed(ctx, o, e)
		}
	}
	o.Phase = "complete"
	o.ReasonCode = ""
	if e := s.saveOperation(ctx, o); e != nil {
		return e
	}
	delete(s.activeLogins, o.ID)
	s.invalidateAccount(o.AccountID)
	s.notifyDefaultChanged(o.Provider)
	return nil
}
func (s *Service) invalidateAccount(id string) { s.mu.Lock(); delete(s.readings, id); s.mu.Unlock() }

// Logout disables new uses immediately and keeps metadata and bindings even
// when native logout fails. A repeated request resumes the same durable work.
func (s *Service) Logout(ctx context.Context, id string) error {
	return s.mutateAccount(ctx, id, "logout")
}

// Remove retires metadata only after confirmed native logout, then cleans locally.
func (s *Service) Remove(ctx context.Context, id string) error {
	return s.mutateAccount(ctx, id, "remove")
}
func (s *Service) mutateAccount(ctx context.Context, id, action string) error {
	if runtime.GOOS != "linux" {
		return ports.ErrAgentAccountStorageUnsupported
	}
	s.opMu.Lock()
	defer s.opMu.Unlock()
	release, e := s.acquireProfileMutation(id)
	if e != nil {
		return e
	}
	defer release()
	a, e := s.find(ctx, id)
	if e != nil {
		return e
	}
	if a.Device() {
		return ports.ErrAgentAccountDeviceFixed
	}
	if o, found, e := s.activeOperation(ctx, id); e != nil {
		return e
	} else if found {
		if o.Action != action {
			return &OperationError{OperationID: o.ID, category: ports.ErrAgentAccountOperationBusy}
		}
		return s.resumeOperationLocked(ctx, &o)
	}
	if a.RetiredAt != nil {
		return nil
	}
	if action == "remove" {
		// A new remove is refused before journal/disable/stop effects. Existing
		// previously-confirmed operations above remain recoverable after upgrade.
		if err := s.refuseLiveOwners(ctx, id); err != nil {
			return err
		}
	}
	if action == "logout" && a.DisabledAt != nil && a.LifecycleOperationID != "" {
		prior, err := s.operation(ctx, a.LifecycleOperationID)
		if err != nil {
			return err
		}
		if prior.Phase == "complete" && prior.Action == "logout" && prior.Outcome == "signed-out" {
			return nil
		}
	}

	if e := s.checkAccountProfile(a.Home); e != nil {
		return e
	}

	o := domain.AgentAccountOperation{ID: uuid.NewString(), AccountID: id, Provider: a.Provider, Action: action, Phase: "closing", Outcome: "signed-out", WasDisabled: a.DisabledAt != nil, CreatedAt: s.now(), UpdatedAt: s.now()}
	if action == "remove" {
		o.Outcome = "removed"
	}
	j, e := s.journal()
	if e != nil {
		return e
	}
	if e = j.CreateAgentAccountOperation(ctx, o); e != nil {
		return ports.ErrAgentAccountStorageUnavailable
	}
	return s.resumeOperationLocked(ctx, &o)
}

// Recover runs in the background at startup and periodically thereafter.
// A previous-process pending login is cancelled; accepted logins are completed.
// In-process successful child exit verifies the login instead of cancelling it.
// Existing saved profiles survive cancellation and all failures stay retryable.
func (s *Service) Recover(ctx context.Context) error {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	j, e := s.journal()
	if e != nil {
		return e
	}
	rows, e := j.ListAgentAccountOperations(ctx)
	if e != nil {
		return ports.ErrAgentAccountStorageUnavailable
	}
	var errs []error
	var savedAccounts map[string]bool
	var catalogErr error
	catalogLoaded := false
	for _, o := range rows {
		if s.retries[o.ID].next.After(s.now()) {
			continue
		}
		if o.Phase == "complete" {
			if o.Action != "login" || o.Outcome != "cancelled" {
				continue
			}
			// Older in-flight Prepare could recreate an already cancelled new
			// profile. Repair only row-less new-login homes, never saved accounts.
			// One snapshot per sweep avoids rescanning the catalog for every
			// completed cancellation. opMu excludes concurrent lifecycle writes.
			if !catalogLoaded {
				catalogLoaded = true
				var accounts []domain.AgentAccount
				accounts, catalogErr = s.store.ListAgentAccounts(ctx)
				if catalogErr != nil {
					errs = append(errs, ports.ErrAgentAccountStorageUnavailable)
				}
				savedAccounts = make(map[string]bool, len(accounts))
				for _, account := range accounts {
					savedAccounts[account.ID] = true
				}
			}
			if catalogErr != nil || savedAccounts[o.AccountID] {
				continue
			}
			home := filepath.Join(s.root, string(o.Provider), o.AccountID)
			if _, err := os.Lstat(home); errors.Is(err, os.ErrNotExist) {
				continue
			} else if err != nil {
				errs = append(errs, ports.ErrAgentAccountStorageUnavailable)
				continue
			}
			o.Phase = "closing"
			if err := s.saveOperation(ctx, &o); err != nil {
				errs = append(errs, err)
				continue
			}
		}
		if o.Phase == "pending" && s.activeLogins[o.ID] {
			s.mu.Lock()
			term := s.terminals
			s.mu.Unlock()
			alive, err := term.CommandTerminalAlive(ctx, o.TerminalID)
			if err != nil || alive {
				continue
			}
			// Device-auth CLI exits on success. Use the same acceptance and
			// duplicate/mismatch rules as explicit verify; Settings may be closed.
			status, _, err := s.verifyLogin(ctx, o.ID)
			if err != nil {
				errs = append(errs, err)
			} else if status == LoginWaiting {
				// Only a successful native probe may establish that a finished
				// child left no subscription login. Unknown probes stay retryable.
				if err := s.cancelLogin(ctx, o.ID); err != nil {
					errs = append(errs, err)
				}
			}
			continue
		}
		if o.Phase == "pending" {
			o.Phase = "closing"
			o.Outcome = "cancelled"
			if e := s.saveOperation(ctx, &o); e != nil {
				errs = append(errs, e)
				continue
			}
		}
		if e = s.resumeOperation(ctx, &o); e != nil {
			errs = append(errs, e)
		}
	}
	return errors.Join(errs...)
}

// StartRecovery reconciles interrupted operations at boot and retries every 30 seconds.
func (s *Service) StartRecovery(ctx context.Context) {
	// Pending/accepted operations already disable their saved accounts durably.
	// Native cleanup may be slow; it must not hold daemon boot hostage.
	go func() {
		if e := s.Recover(ctx); e != nil {
			s.log.Warn("account lifecycle recovery incomplete", "reasonCode", "account_cleanup_unconfirmed")
		}
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = s.Recover(ctx)
			}
		}
	}()
}

// Operation returns durable non-secret progress for local reconnect/retry.
func (s *Service) Operation(ctx context.Context, id string) (domain.AgentAccountOperation, error) {
	return s.operation(ctx, id)
}
