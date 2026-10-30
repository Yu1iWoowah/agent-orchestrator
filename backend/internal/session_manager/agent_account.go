package sessionmanager

import (
	"context"
	"errors"
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func (m *Manager) beginAccountUses(ctx context.Context, ids ...string) (func(), error) {
	if guard, ok := m.agentAccounts.(ports.AgentAccountUseGuard); ok {
		return guard.AcquireAccountUses(ctx, ids...)
	}
	return func() {}, nil
}

// Share ownership transfer with the existing harness guard, including async
// Chat launches whose request returns before native startup has completed.
func (m *Manager) beginHarnessAndAccountUse(ctx context.Context, harness domain.AgentHarness, accountID string) (func(), error) {
	releaseHarness, err := m.beginHarnessUse(harness)
	if err != nil {
		return nil, err
	}
	releaseAccount, err := m.beginAccountUses(ctx, accountID)
	if err != nil {
		releaseHarness()
		return nil, err
	}
	return func() { releaseAccount(); releaseHarness() }, nil
}

func (m *Manager) boundAccountForUse(ctx context.Context, rec domain.SessionRecord) (string, error) {
	id := m.accountOverride(rec.ID)
	if id == "" {
		if selected, ok := ports.AgentAccountFromContext(ctx); ok {
			if p, supported := domain.AgentAccountProviderFor(rec.Harness); supported && selected.Provider == p {
				id = selected.AccountID
			}
		}
	}
	if id == "" && m.agentAccounts != nil {
		return m.agentAccounts.SessionAccountID(ctx, rec.ID, rec.Harness)
	}
	return id, nil
}
func (m *Manager) beginBoundHarnessUse(ctx context.Context, rec domain.SessionRecord) (func(), error) {
	id, err := m.boundAccountForUse(ctx, rec)
	if err != nil {
		return nil, err
	}
	return m.beginHarnessAndAccountUse(ctx, rec.Harness, id)
}

// AccountUsageOwners augments durable bindings with launches whose execution
// has not yet reported activity. A failed probe is never evidence of no owner.
func (m *Manager) AccountUsageOwners(ctx context.Context, id string) ([]domain.AgentAccountUsageOwner, error) {
	store, ok := m.store.(ports.AgentAccountUsageStore)
	if !ok {
		return nil, ports.ErrAgentAccountStorageUnavailable
	}
	owners, err := store.ListAgentAccountUsageOwners(ctx, id)
	if err != nil {
		return nil, ports.ErrAgentAccountStorageUnavailable
	}
	for i := range owners {
		if owners[i].Kind == "session" && m.launchInFlight(owners[i].SessionID) {
			owners[i].Blocking = true
		}
	}
	return owners, nil
}

// StartAccountTransition moves the session's agent to accountID through the
// same durable handoff as an interface switch, so a move either completes or
// leaves the session on its old account:
//
//  1. While the current agent keeps running, everything that can fail without
//     touching the conversation is checked: the account's login, launch auth
//     in its environment, and that its folder can see the conversation.
//  2. The current turn drains (or, with the interrupt policy, is cancelled) and
//     the agent stops; a provider allows one live writer per conversation.
//  3. The same conversation resumes under the new account.
//  4. Only then does the session's binding change. A failure before that stops
//     whatever started and restarts the agent on the old account.
//
// The caller gets the transition at once; the handoff runs in the background.
func (m *Manager) StartAccountTransition(
	ctx context.Context,
	id domain.SessionID,
	accountID string,
	policy domain.SessionInterfaceTransitionPolicy,
) (domain.SessionInterfaceTransition, error) {
	ctx, releaseReplacement, err := m.beginReplacementSessionAction(ctx, id)
	if err != nil {
		return domain.SessionInterfaceTransition{}, err
	}
	defer releaseReplacement()
	if !policy.Valid() {
		return domain.SessionInterfaceTransition{}, fmt.Errorf("transition policy %q is invalid", policy)
	}
	if m.agentAccounts == nil {
		return domain.SessionInterfaceTransition{}, ports.ErrAgentAccountUnknown
	}
	store, ok := m.store.(interfaceTransitionStore)
	if !ok {
		return domain.SessionInterfaceTransition{}, ErrInterfaceHandoffUnsupported
	}
	rec, found, err := m.store.GetSession(ctx, id)
	if err != nil {
		return domain.SessionInterfaceTransition{}, err
	}
	if !found {
		return domain.SessionInterfaceTransition{}, ErrNotFound
	}
	if rec.IsTerminated {
		return domain.SessionInterfaceTransition{}, ErrTerminated
	}
	if interfaceWorkspaceUnavailable(rec) {
		return domain.SessionInterfaceTransition{}, fmt.Errorf("%w: session %s", ErrInterfaceWorkspaceUnavailable, id)
	}
	current, err := m.agentAccounts.SessionAccountID(ctx, id, rec.Harness)
	if err != nil {
		return domain.SessionInterfaceTransition{}, err
	}
	if current == "" {
		return domain.SessionInterfaceTransition{}, ports.ErrAgentAccountProviderMismatch
	}
	if current == accountID {
		return domain.SessionInterfaceTransition{}, ports.ErrAgentAccountUnchanged
	}
	// Refuse an unusable account before claiming the session, so a wrong pick
	// never interrupts the running agent.
	if err := m.agentAccounts.ValidateAccount(ctx, rec.Harness, accountID); err != nil {
		return domain.SessionInterfaceTransition{}, err
	}
	nativeID, err := m.handoffNativeConversationID(ctx, rec)
	if err != nil {
		return domain.SessionInterfaceTransition{}, err
	}
	mode := domain.NormalizeSessionMode(rec.Mode)
	now := m.clock()
	transition, created, err := store.CreateSessionInterfaceTransition(ctx, domain.SessionInterfaceTransition{
		ID: m.newLaunchID(), SessionID: id, SourceMode: mode, TargetMode: mode,
		Policy: policy, HistoryPolicy: domain.SessionInterfaceTransitionHistoryStrict,
		Phase: domain.SessionInterfaceTransitionRequested, NativeConversationID: nativeID,
		SourceAccountID: current, TargetAccountID: accountID, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		return domain.SessionInterfaceTransition{}, err
	}
	if !created {
		return transition, ErrInterfaceTransitionInProgress
	}
	if err := m.launchInterfaceTransition(ctx, transition); err != nil {
		return domain.SessionInterfaceTransition{}, err
	}
	return transition, nil
}

// preflightAccountTarget checks, while the source still runs, that the target
// account can resume this conversation.
func (m *Manager) preflightAccountTarget(
	ctx context.Context,
	rec domain.SessionRecord,
	transition domain.SessionInterfaceTransition,
) error {
	if domain.NormalizeSessionMode(rec.Mode) == domain.SessionModeTUI {
		if _, ok := m.store.(accountTransitionRuntimeStore); !ok {
			return ErrInterfaceHandoffUnsupported
		}
	}
	if validator, ok := m.agentAccounts.(ports.AgentAccountHandoffValidator); ok {
		if err := validator.ValidateAccountHandoff(ctx, rec.Harness, transition.SourceAccountID, transition.TargetAccountID, transition.NativeConversationID); err != nil {
			return err
		}
	}
	if err := m.agentAccounts.ValidateAccount(ctx, rec.Harness, transition.TargetAccountID); err != nil {
		return err
	}
	selected, err := m.agentAccounts.ResolveAccount(ctx, ports.AgentAccountResolutionRequest{Harness: rec.Harness, AccountID: transition.TargetAccountID})
	if err != nil {
		return err
	}
	ctx = ports.WithAgentAccount(ctx, selected)
	agent, ok := m.agents.Agent(rec.Harness)
	if !ok {
		return ErrUnknownHarness
	}
	project, err := m.loadProject(ctx, rec.ProjectID)
	if err != nil {
		return err
	}
	env, err := m.runtimeEnvOnAccount(rec, project.Config.Env, transition.TargetAccountID)
	if err != nil {
		return err
	}
	config := restoredAgentConfig(rec, project.Config)
	if err := m.preflightAccountModels(ctx, rec, config); err != nil {
		return err
	}
	pinRuntimePermissionEnv(env, config.Permissions)
	m.augmentAgentRuntimeEnv(agent, env)
	if err := m.preflightLaunchAuth(ctx, agent, rec, env); err != nil {
		return err
	}
	// Managed profiles share only AO-owned history. A device/external thread is
	// not implicitly imported; require positive visibility before stopping it.
	if transition.NativeConversationID != "" {
		probe, ok := agent.(ports.AgentInterfaceHandoffHistoryProbe)
		if !ok {
			return ErrNativeConversationUnverified
		}
		{
			exists, probeErr := probe.NativeConversationExists(ctx, ports.SessionRef{
				ID: string(rec.ID), WorkspacePath: rec.Metadata.WorkspacePath,
				Metadata: map[string]string{ports.MetadataKeyAgentSessionID: transition.NativeConversationID},
			}, transition.NativeConversationID, env)
			if probeErr != nil {
				return ErrNativeConversationUnverified
			}
			if !exists {
				return fmt.Errorf("%w: the account's folder cannot see this conversation", ErrNativeConversationMissing)
			}
		}
	}
	if domain.NormalizeSessionMode(rec.Mode) == domain.SessionModeChat {
		if m.chat == nil {
			return ports.ErrChatUnsupported
		}
		return m.chat.PreflightChat(ctx, rec.Harness, config.Permissions)
	}
	return m.preflightTerminalLaunch(ctx, agent, rec, project, transition.NativeConversationID, env)
}

func (m *Manager) preflightAccountModels(ctx context.Context, rec domain.SessionRecord, config ports.AgentConfig) error {
	// With no explicit settings the provider chooses its defaults, just as at spawn.
	if config.Model == "" && config.Effort == "" && config.FastMode != domain.AgentFastOn {
		return nil
	}
	covers := func(catalog ports.AgentModelCatalog) bool {
		if savedCatalogCovers(catalog, config.Model, config.Effort) {
			return true
		}
		// Direct-entry models have no advertised capabilities. Accept the name,
		// but never infer support for an explicit effort or fast requirement.
		return catalog.AllowCustom && config.Model != "" && config.Effort == "" && config.FastMode != domain.AgentFastOn &&
			!catalog.Stale && (catalog.RefreshState == "" || catalog.RefreshState == "idle")
	}
	if m.modelCatalog == nil {
		return ports.ErrModelCapabilitiesUnavailable
	}
	catalog, err := m.modelCatalog.Models(ctx, string(rec.Harness), string(rec.ProjectID), false)
	if err != nil || !covers(catalog) {
		catalog, err = m.modelCatalog.Models(ctx, string(rec.Harness), string(rec.ProjectID), true)
	}
	if err != nil || !covers(catalog) {
		return ports.ErrModelCapabilitiesUnavailable
	}
	if config.FastMode == domain.AgentFastOn {
		for _, model := range catalog.Models {
			if model.ID == config.Model || config.Model == "" && model.IsDefault {
				if !model.SupportsFast {
					return ports.ErrAgentSettingsUnsupported
				}
				break
			}
		}
	}
	return nil
}

// markAccountSwitch tells the conversation that a move which stopped the turn
// in progress is why that turn ended, so the chat shows the switch instead of
// a failure. Best-effort: the move has already committed.
func (m *Manager) markAccountSwitch(ctx context.Context, rec domain.SessionRecord, transition domain.SessionInterfaceTransition) {
	if transition.Policy != domain.SessionInterfaceTransitionInterrupt || domain.NormalizeSessionMode(rec.Mode) != domain.SessionModeChat {
		return
	}
	marker, ok := m.chat.(interface {
		MarkAccountSwitch(context.Context, domain.SessionID, string) error
	})
	if !ok {
		return
	}
	if err := marker.MarkAccountSwitch(ctx, rec.ID, transition.TargetAccountID); err != nil {
		m.logger.Warn("account transition: stopped turn not marked as switched", "sessionID", rec.ID, "transition", transition.ID, "error", err)
	}
}

// runtimeEnvOnAccount is runtimeEnv as the session's agent would get it on
// accountID, for checks made before the move. It leaves the account override
// alone: the source agent still runs, and a restart of it must keep its own
// account. An account env that cannot be resolved fails the check: the env
// would name another login, and every probe below would pass against it.
func (m *Manager) runtimeEnvOnAccount(rec domain.SessionRecord, projectEnv map[string]string, accountID string) (map[string]string, error) {
	return m.runtimeEnvChecked(rec.ID, rec.ProjectID, rec.IssueID, projectEnv, accountID, rec.Harness)
}

// ExitAgentsOnAccount stops every running agent that uses accountID, before the
// account is removed. Each session stays, its agent exited; opening it resumes
// it only after an explicit account choice; its historical binding remains.
func (m *Manager) ExitAgentsOnAccount(ctx context.Context, accountID string) error {
	if m.agentAccounts == nil {
		return nil
	}
	if store, ok := m.store.(interfaceTransitionStore); ok {
		active, err := store.ListActiveSessionInterfaceTransitions(ctx)
		if err != nil {
			return ports.ErrAgentAccountStorageUnavailable
		}
		for _, transition := range active {
			// Before commit the target has no session binding. Its durable
			// intent is nevertheless an owner, including uncertain shutdown.
			if transition.AccountMove() && (transition.SourceAccountID == accountID || transition.TargetAccountID == accountID) {
				return ports.ErrAgentAccountInUse
			}
		}
	}
	recs, err := m.store.ListAllSessions(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, rec := range recs {
		if rec.IsTerminated {
			continue
		}
		bound, err := m.agentAccounts.SessionAccountID(ctx, rec.ID, rec.Harness)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if bound != accountID {
			continue
		}
		// A launch already underway (a resume, a spawn, a move) may have read the
		// account's folder before the removal began; the session still reads as
		// exited until its agent reports. Refuse rather than delete the folder
		// under it: a retry after it settles stops it like any running agent.
		if m.launchInFlight(rec.ID) {
			errs = append(errs, fmt.Errorf("session %s is starting its agent on the account", rec.ID))
			continue
		}
		if rec.Activity.State == domain.ActivityExited {
			continue
		}
		if _, err := m.ExitAgent(ctx, rec.ID); err != nil && !errors.Is(err, ErrAgentExited) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// launchInFlight reports an agent operation, a transition or an asynchronous
// chat spawn that may still be starting an agent for the session.
func (m *Manager) launchInFlight(id domain.SessionID) bool {
	if m.SessionMutationInProgress(id) {
		return true
	}
	m.asyncChatSpawnsMu.Lock()
	defer m.asyncChatSpawnsMu.Unlock()
	_, starting := m.asyncChatSpawns[id]
	return starting
}

// The account override makes the session's launches use accountID instead of
// its binding while an account transition starts the agent there, before the
// binding commits. Transitions own the session, so only their launch reads it.
func (m *Manager) setAccountOverride(id domain.SessionID, accountID string) {
	m.accountOverrideMu.Lock()
	defer m.accountOverrideMu.Unlock()
	if m.accountOverrides == nil {
		m.accountOverrides = map[domain.SessionID]string{}
	}
	m.accountOverrides[id] = accountID
}

func (m *Manager) clearAccountOverride(id domain.SessionID) {
	m.accountOverrideMu.Lock()
	defer m.accountOverrideMu.Unlock()
	delete(m.accountOverrides, id)
}

func (m *Manager) accountOverride(id domain.SessionID) string {
	m.accountOverrideMu.Lock()
	defer m.accountOverrideMu.Unlock()
	return m.accountOverrides[id]
}
