package ports

import (
	"context"
	"errors"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// AgentAccountReviewerInUseError identifies a local execution that must stop
// before cleanup. It carries only opaque references, never provider output.
type AgentAccountReviewerInUseError struct {
	ReviewID    string
	SessionName string
	PRURL       string
	SessionID   domain.SessionID
	Harness     domain.ReviewerHarness
}

func (*AgentAccountReviewerInUseError) Error() string { return ErrAgentAccountInUse.Error() }
func (*AgentAccountReviewerInUseError) Unwrap() error { return ErrAgentAccountInUse }

// Agent account errors the API maps to client responses.
var (
	ErrAgentAccountLegacyIdentityConflict     = errors.New("account_legacy_identity_conflict")
	ErrAgentAccountLocalOnly                  = errors.New("account_local_only")
	ErrAgentAccountUnavailableHomeOccupied    = errors.New("account_unavailable_home_occupied")
	ErrAgentAccountCleanupUnconfirmed         = errors.New("account_cleanup_unconfirmed")
	ErrAgentAccountOperationBusy              = errors.New("account_operation_busy")
	ErrAgentAccountCredentialStoreUnsupported = errors.New("account_credential_store_unsupported")

	ErrAgentAccountStorageUnsafe      = errors.New("account_storage_unsafe")
	ErrAgentAccountStorageUnavailable = errors.New("account_storage_unavailable")
	ErrAgentAccountStorageUnsupported = errors.New("account_storage_unsupported")
	ErrAgentAccountUnknown            = errors.New("agent account not found")
	ErrAgentAccountProviderMismatch   = errors.New("the account belongs to another provider")
	ErrAgentAccountSignedOut          = errors.New("the account's config folder is not signed in")
	ErrAgentAccountDuplicate          = errors.New("that login is already an account")
	ErrAgentAccountLoginUnknown       = errors.New("agent account sign-in not found")
	ErrAgentAccountDeviceFixed        = errors.New("the computer's own login cannot be removed here")
	ErrAgentAccountUnchanged          = errors.New("the session already uses this account")
	ErrAgentAccountChanged            = errors.New("the session's account changed since this turn")
	ErrAgentAccountUnshared           = errors.New("the account's folder cannot link to this computer's history")
	ErrAgentAccountInUse              = errors.New("an agent that uses the account could not be stopped")
	ErrAgentAccountDefaultUnusable    = errors.New("the default account is signed out or now holds another login")
)

// AgentAccountUnavailableHomeOccupiedMessage identifies the reserved namespace
// without exposing an absolute account path or suggesting deletion of user data.
const AgentAccountUnavailableHomeOccupiedMessage = "AO's reserved unavailable account directory is occupied or cannot be safely inspected. Stop related child tools, review and move any data out of .unavailable under AO account storage, then retry. AO only removes empty reserved folders automatically."

// AgentAccountProviderCLI reads and prepares one provider's CLI config
// folders. home is empty for the computer's own folder. Implementations ask the
// CLI itself; none of them reads a token.
type AgentAccountProviderCLI interface {
	Provider() domain.AgentAccountProvider
	// Identity reports who home is signed in to.
	Identity(ctx context.Context, home string) (domain.AgentAccountIdentityCheck, error)
	// Limits reports the subscription limits of the account signed in to home.
	Limits(ctx context.Context, home string) (domain.AgentAccountLimits, error)
	// LoginArgv is the command that signs home in, run in a terminal with LaunchEnv(home).
	LoginArgv(ctx context.Context) ([]string, error)
	// Logout signs home out through the CLI.
	Logout(ctx context.Context, home string) error
	// Prepare links new profiles only to AO-owned conversation history.
	// Credentials, settings, caches and IPC remain private to the profile.
	Prepare(home string) error
	// LaunchEnv is the environment that makes the CLI use home.
	LaunchEnv(home string) map[string]string
}

// AgentAccountStore persists accounts, the default per provider, and which
// account each session uses.
type AgentAccountStore interface {
	ListAgentAccounts(ctx context.Context) ([]domain.AgentAccount, error)
	CreateAgentAccount(ctx context.Context, account domain.AgentAccount) error
	DeleteAgentAccount(ctx context.Context, id string) error
	GetAgentAccountDefault(ctx context.Context, provider domain.AgentAccountProvider) (string, bool, error)
	SetAgentAccountDefault(ctx context.Context, provider domain.AgentAccountProvider, accountID string) error
	// BindSessionAgentAccount records that the session uses accountID for the
	// provider from now on. Earlier bindings are kept for usage attribution.
	BindSessionAgentAccountWithSource(context.Context, domain.SessionID, domain.ResolvedAgentAccount, time.Time) error
	GetSessionAgentAccountBinding(context.Context, domain.SessionID, domain.AgentAccountProvider) (domain.ResolvedAgentAccount, bool, error)
	BindSessionAgentAccount(ctx context.Context, sessionID domain.SessionID, provider domain.AgentAccountProvider, accountID string, at time.Time) error
	GetSessionAgentAccount(ctx context.Context, sessionID domain.SessionID, provider domain.AgentAccountProvider) (string, bool, error)
	// AgentAccountUsageSince sums each account's model usage since the time,
	// attributing every usage event to the account its session used then.
	AgentAccountUsageSince(ctx context.Context, since time.Time) ([]domain.AgentAccountUsage, error)
}

// AgentAccountLaunchEnv gives Session Manager the account environment of a
// session's agent launch.
type AgentAccountLaunchEnv interface {
	AgentAccountResolver
	BindResolvedSession(context.Context, domain.SessionID, domain.AgentHarness, domain.ResolvedAgentAccount) error
	// SessionLaunchEnv returns the variables that run each provider's CLI under
	// the session's account, binding the provider's default first when the
	// session has none yet. Validation errors block only harness's provider;
	// other providers retain their pinned environments.
	SessionLaunchEnv(ctx context.Context, sessionID domain.SessionID, harness domain.AgentHarness) (map[string]string, error)
	// SessionLaunchEnvWith is SessionLaunchEnv with accountID in place of the
	// session's account for its provider. It binds nothing.
	SessionLaunchEnvWith(ctx context.Context, sessionID domain.SessionID, accountID string, harness domain.AgentHarness) (map[string]string, error)
	// BindSession pins the session to accountID, or to the provider's default
	// when accountID is empty.
	BindSession(ctx context.Context, sessionID domain.SessionID, harness domain.AgentHarness, accountID string) error
	// CommitSession records that the session now uses accountID, with no
	// sign-in probe: a move calls it once its agent already runs there.
	CommitSession(ctx context.Context, sessionID domain.SessionID, harness domain.AgentHarness, accountID string) error
	// SessionAccountID is the account the session uses for harness's provider,
	// or "" when the harness has no accounts.
	SessionAccountID(ctx context.Context, sessionID domain.SessionID, harness domain.AgentHarness) (string, error)
	// SessionAccountBinding reads only the durable execution choice; it never
	// resolves current defaults when the binding is missing.
	SessionAccountBinding(context.Context, domain.SessionID, domain.AgentHarness) (domain.ResolvedAgentAccount, error)
	// ValidateAccount checks that accountID can run harness now: it is of the
	// harness's provider and signed in as the login it was added with.
	ValidateAccount(ctx context.Context, harness domain.AgentHarness, accountID string) error
}

// AgentAccountJournal persists lifecycle intent before any provider/PTY effect.
// CommitAgentAccountLogin atomically records a disabled account and accepted
// operation; completing it enables the account only after PTY death is proven.
type AgentAccountJournal interface {
	CreateAgentAccountOperation(context.Context, domain.AgentAccountOperation) error
	UpdateAgentAccountOperation(context.Context, domain.AgentAccountOperation) error
	GetAgentAccountOperation(context.Context, string) (domain.AgentAccountOperation, bool, error)
	ListAgentAccountOperations(context.Context) ([]domain.AgentAccountOperation, error)
	CommitAgentAccountLogin(context.Context, domain.AgentAccountOperation, domain.AgentAccount) error
}

// AgentAccountResolutionRequest carries a daemon-owned project snapshot. Only
// AccountID is a caller override; callers cannot supply a resolved identity.
type AgentAccountResolutionRequest struct {
	Harness       domain.AgentHarness
	Role          domain.AgentAccountRole
	ProjectConfig domain.ProjectConfig
	AccountID     string
}

// AgentAccountResolver is shared by session, automation and reviewer execution.
// Resolution validates the exact selected account and never binds or falls back.
type AgentAccountResolver interface {
	ResolveAccount(context.Context, AgentAccountResolutionRequest) (domain.ResolvedAgentAccount, error)
}

// AgentAccountUseGuard fences profile mutation across native preflight, launch
// and durable ownership recording. Release must run after ownership is saved or
// conclusively stopped. Callers still validate the exact execution identity.
type AgentAccountUseGuard interface {
	AcquireAccountUses(context.Context, ...string) (func(), error)
}

// AgentAccountUsageStore lists current and historical local account owners.
type AgentAccountUsageStore interface {
	ListAgentAccountUsageOwners(context.Context, string) ([]domain.AgentAccountUsageOwner, error)
}

// ReviewerExecutionStore creates a fresh immutable owner only from the exact
// latest archived reviewer, without moving its previous bindings or runs.
type ReviewerExecutionStore interface {
	GetReviewByID(context.Context, string) (domain.Review, bool, error)
	GetReviewBySessionAndHarness(context.Context, domain.SessionID, domain.ReviewerHarness) (domain.Review, bool, error)
	CreateReviewerAccountExecution(context.Context, string, string, domain.ResolvedAgentAccount, time.Time) (domain.Review, bool, error)
}

// AgentAccountInUseError describes a refused mutation. No operation has been
// scheduled and no account was disabled; callers can show the affected owners.
type AgentAccountInUseError struct {
	Owners []domain.AgentAccountUsageOwner
}

func (*AgentAccountInUseError) Error() string { return ErrAgentAccountInUse.Error() }
func (*AgentAccountInUseError) Unwrap() error { return ErrAgentAccountInUse }

// ReviewerAgentAccountStore keeps reviewer identities separate from worker
// session bindings. First write wins for each provider/execution pair.
type ReviewerAgentAccountStore interface {
	BindReviewerAgentAccount(context.Context, string, domain.ResolvedAgentAccount, time.Time) error
	GetReviewerAgentAccount(context.Context, string, domain.AgentAccountProvider) (domain.ResolvedAgentAccount, bool, error)
}

// ReviewerAccountBindings pins each reviewer execution separately from its
// worker; changing defaults never changes a pass or restored conversation.
type ReviewerAccountBindings interface {
	AgentAccountExecution
	ResolveReviewerAccount(context.Context, string, AgentAccountResolutionRequest) (domain.ResolvedAgentAccount, error)
	ReviewerLaunchEnv(context.Context, string, domain.AgentHarness) (map[string]string, error)
}

// AgentAccountHandoffValidator prevents implicit imports of device/external
// history and refreshes managed target identity before the source is stopped.
type AgentAccountHandoffValidator interface {
	ValidateAccountHandoff(context.Context, domain.AgentHarness, string, string, string) error
}
