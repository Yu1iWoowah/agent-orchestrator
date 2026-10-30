package sessionmanager

import (
	"context"
	"errors"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

const agentOperationSettings agentOperationKind = "agent_settings"

const (
	unconfirmedTUIWriterWorkspaceReason = "unconfirmed native writer; workspace retained"
	unavailableTUIWriterWorkspaceReason = "native writer status unavailable; workspace retained"
)

type tuiSettingsStore interface {
	SetTUIAgentSettingsRuntimeUnknown(context.Context, domain.SessionID, string, bool) error
	PinAgentSettingsExecution(context.Context, domain.SessionID, string, int64, domain.AgentSettingsExecution, time.Time) (domain.AgentSettingsOperation, error)
	RecordTUIAgentSettingsRuntime(context.Context, domain.SessionID, string, string, string, bool) error
	interfaceTransitionStore
	SessionAgentSettings(context.Context, domain.SessionID) (domain.SessionAgentSettings, error)
	LatestAppliedAgentSettingsOperation(context.Context, domain.SessionID) (domain.AgentSettingsOperation, bool, error)
	BeginAgentSettingsOperation(context.Context, domain.AgentSettingsOperation) (domain.AgentSettingsOperation, error)
	AgentSettingsOperation(context.Context, domain.SessionID, string) (domain.AgentSettingsOperation, error)
	AdvanceAgentSettingsOperation(context.Context, domain.SessionID, string, int64, domain.AgentSettingsCheckpoint, time.Time) (domain.AgentSettingsOperation, error)
	AdvanceAgentSettingsRecovery(context.Context, domain.SessionID, string, int64, domain.AgentSettingsCheckpoint, domain.AgentSettingsRecovery, time.Time) (domain.AgentSettingsOperation, error)
	FinishAgentSettingsOperationWithCode(context.Context, domain.SessionID, string, int64, domain.AgentSettingsPhase, domain.AgentSettingsErrorCategory, string, time.Time) (domain.AgentSettingsOperation, error)
	CommitAgentSettingsOperation(context.Context, domain.SessionID, string, time.Time) (domain.AgentSettingsOperation, error)
}

type tuiSettingsLaunchKey struct{}
type explicitTUISettingsRecoveryKey struct{}
type tuiSettingsLaunch struct {
	settings         domain.AgentSettings
	sessionID        domain.SessionID
	accountID        string
	nativeID         string
	handle           ports.RuntimeHandle
	attempted        bool
	noEffects        bool
	operationID      string
	targetGeneration string
	rollback         bool
	native           *domain.AgentSettingsNativeValues
	protocol         string
}

func (s *tuiSettingsLaunch) capture(handle ports.RuntimeHandle, err error) {
	s.handle = handle
	var effect ports.RuntimeEffectError
	if errors.As(err, &effect) {
		s.noEffects = effect.EffectOutcome() == ports.RuntimeEffectNone || effect.CleanupOutcome() == ports.RuntimeCleanupSucceeded
		if s.handle.ID == "" {
			s.handle = effect.PossibleHandle()
		}
	}
}
func applyTUISettingsLaunch(ctx context.Context, cfg *domain.AgentConfig) {
	if state, _ := ctx.Value(tuiSettingsLaunchKey{}).(*tuiSettingsLaunch); state != nil {
		cfg.Model = string(state.settings.Model)
		cfg.Effort = string(state.settings.Effort)
		cfg.Launch.FastMode = state.settings.FastMode == domain.AgentFastOn
		cfg.Launch.FastModeOff = state.settings.FastMode == domain.AgentFastOff
		if state.native != nil {
			cfg.Model, cfg.Effort = state.native.Model, state.native.Effort
			if state.protocol == "codex-native-resume" {
				cfg.Launch.NativeServiceTier = state.native.Fast
			}
		}
	}
}

// SetTUIAgentSettingsValidator completes daemon startup before mutation opens.
func (m *Manager) SetTUIAgentSettingsValidator(validator ports.ChatAgentSettingsValidator) {
	m.tuiSettingsValidator = validator
}

func settingsOwner(current domain.SessionAgentSettings) domain.AgentSettingsOwner {
	return domain.AgentSettingsOwner{SessionID: current.SessionID, AccountID: current.AccountID, ControllerGeneration: current.OwnerGeneration, ProviderConversationID: current.ProviderConversationID, BranchID: current.BranchID}
}

func (m *Manager) validateTUISettings(ctx context.Context, store tuiSettingsStore, op domain.AgentSettingsOperation, restoring bool) error {
	current, err := store.SessionAgentSettings(ctx, op.SessionID)
	if err != nil {
		return err
	}
	if (current.IsTerminated && (!restoring || op.Phase != domain.AgentSettingsApplied)) || current.AccountID != op.AccountID || current.ProviderConversationID != op.ProviderConversationID || current.Revision != op.BaseRevision || current.Settings != op.Source {
		return ports.ErrAgentSettingsConflict
	}
	// Restoring durable preferences is not a new capability choice. Discovery
	// failure must not strand a native conversation with qualified saved wire.
	if restoring {
		return nil
	}
	if m.tuiSettingsValidator == nil {
		return ports.ErrAgentSettingsCapabilitiesUnavailable
	}
	qualification, err := m.tuiSettingsValidator.ValidateAgentSettings(ctx, op, settingsOwner(current), restoring)
	if err == nil && !restoring && op.Execution != nil && qualification.InputFingerprint != op.Execution.Qualification.InputFingerprint {
		return ports.ErrAgentSettingsCapabilitiesUnavailable
	}
	return err
}

// QueueTUIAgentSettings only admits an explicit same-conversation restart.
// The physical handoff is created atomically by BeginAgentSettingsOperation.
func (m *Manager) QueueTUIAgentSettings(ctx context.Context, op domain.AgentSettingsOperation) (domain.AgentSettingsOperation, error) {
	ctx, releaseReplacement, err := m.beginReplacementSessionAction(ctx, op.SessionID)
	if err != nil {
		return domain.AgentSettingsOperation{}, err
	}
	defer releaseReplacement()
	store, ok := m.store.(tuiSettingsStore)
	if !ok || m.tuiSettingsValidator == nil {
		return domain.AgentSettingsOperation{}, ports.ErrAgentSettingsCapabilitiesUnavailable
	}
	if op.ApplyMode != domain.AgentSettingsRestart {
		return domain.AgentSettingsOperation{}, ports.ErrAgentSettingsInvalid
	}
	if err := m.beginAgentOperation(ctx, op.SessionID, agentOperationSettings); err != nil {
		return domain.AgentSettingsOperation{}, ports.ErrAgentSettingsConflict
	}
	handedOff := false
	defer func() {
		if !handedOff {
			m.endAgentOperation(op.SessionID, agentOperationSettings)
		}
	}()
	rec, found, err := m.store.GetSession(ctx, op.SessionID)
	if err != nil {
		return domain.AgentSettingsOperation{}, err
	}
	if !found || rec.IsTerminated || domain.NormalizeSessionMode(rec.Mode) != domain.SessionModeTUI || !rec.Harness.SupportsFastMode() {
		return domain.AgentSettingsOperation{}, ports.ErrAgentSettingsUnsupported
	}
	if interfaceWorkspaceUnavailable(rec) || rec.ProvisionState.IsProvisioning() {
		return domain.AgentSettingsOperation{}, ports.ErrAgentSettingsConflict
	}
	if err := m.validateTUISettings(ctx, store, op, false); err != nil {
		return domain.AgentSettingsOperation{}, err
	}
	if rec.Metadata.RuntimeHandleID != "" {
		if err := m.preflightTUISettings(ctx, rec, op.Target, op.AccountID); err != nil {
			return domain.AgentSettingsOperation{}, err
		}
	}
	accepted, err := store.BeginAgentSettingsOperation(ctx, op)
	if err != nil {
		return domain.AgentSettingsOperation{}, err
	}
	if accepted.TUI == nil {
		return store.CommitAgentSettingsOperation(ctx, accepted.SessionID, accepted.ID, m.clock())
	}
	transition, found, err := store.GetSessionInterfaceTransition(ctx, accepted.ID)
	if err != nil || !found {
		return domain.AgentSettingsOperation{}, ports.ErrAgentSettingsConflict
	}
	runCtx, cancel := context.WithCancel(context.Background())
	run := &interfaceTransitionRun{cancel: cancel, done: make(chan struct{})}
	m.transitionMu.Lock()
	m.transitions[accepted.SessionID] = run
	m.transitionMu.Unlock()
	handedOff = true
	go func() {
		defer close(run.done)
		defer func() {
			m.transitionMu.Lock()
			if m.transitions[accepted.SessionID] == run {
				delete(m.transitions, accepted.SessionID)
			}
			m.transitionMu.Unlock()
		}()
		defer m.endAgentOperation(accepted.SessionID, agentOperationSettings)
		defer m.settleTransitionMessages(transition)
		m.runTUISettings(runCtx, store, accepted, transition)
	}()
	return accepted, nil
}

func (m *Manager) preflightTUISettings(ctx context.Context, rec domain.SessionRecord, settings domain.AgentSettings, accountID string) error {
	agent, ok := m.agents.Agent(rec.Harness)
	if !ok {
		return ErrUnknownHarness
	}
	probe, ok := agent.(ports.AgentInterfaceHandoffHistoryProbe)
	if !ok {
		return ports.ErrAgentSettingsUnsupported
	}
	project, err := m.loadProject(ctx, rec.ProjectID)
	if err != nil {
		return err
	}
	env, err := m.runtimeEnvChecked(rec.ID, rec.ProjectID, rec.IssueID, project.Config.Env, accountID, rec.Harness)
	if err != nil {
		return err
	}
	exists, err := probe.NativeConversationExists(ctx, ports.SessionRef{ID: string(rec.ID), WorkspacePath: rec.Metadata.WorkspacePath}, rec.Metadata.AgentSessionID, env)
	if err != nil || !exists {
		return ports.ErrAgentSettingsNativeHistoryUnavailable
	}
	ctx = context.WithValue(ctx, tuiSettingsLaunchKey{}, &tuiSettingsLaunch{settings: settings})
	return m.preflightTerminalLaunch(ctx, agent, rec, project, rec.Metadata.AgentSessionID, env)
}

func (m *Manager) runTUISettings(ctx context.Context, store tuiSettingsStore, op domain.AgentSettingsOperation, tr domain.SessionInterfaceTransition) {
	// Errors crossing this asynchronous boundary are bounded machine codes; no
	// provider stdout, account environment or grant enters journal/access logs.
	failUnstarted := func(code string) {
		current, err := store.AgentSettingsOperation(context.Background(), op.SessionID, op.ID)
		if err == nil && current.Phase == domain.AgentSettingsPending && current.StartedAt == nil {
			_, _ = store.FinishAgentSettingsOperationWithCode(context.Background(), op.SessionID, op.ID, current.Version, domain.AgentSettingsFailed, domain.AgentSettingsErrorValidation, code, m.clock())
		}
	}
	rec, found, err := m.store.GetSession(ctx, op.SessionID)
	if err != nil || !found {
		failUnstarted("AGENT_SETTINGS_CONFLICT")
		return
	}
	lastInput, release := m.beginTerminalInputDrain(rec)
	if release != nil {
		defer release()
	}
	if err = m.moveInterfaceTransition(op.ID, domain.SessionInterfaceTransitionPreflighting, "", ""); err != nil {
		failUnstarted("AGENT_SETTINGS_CONFLICT")
		return
	}
	if err = m.validateTUISettings(ctx, store, op, false); err != nil {
		failUnstarted("AGENT_SETTINGS_CAPABILITIES_UNAVAILABLE")
		return
	}
	if err = m.preflightTUISettings(ctx, rec, op.Target, op.AccountID); err != nil {
		failUnstarted("AGENT_SETTINGS_NATIVE_HISTORY_UNAVAILABLE")
		return
	}
	if err = m.moveInterfaceTransition(op.ID, domain.SessionInterfaceTransitionDraining, "", ""); err != nil {
		failUnstarted("AGENT_SETTINGS_CONFLICT")
		return
	}
	if err = m.prepareSourceHandoff(ctx, rec, tr.Policy, lastInput); err != nil {
		code := "AGENT_SETTINGS_DRAIN_FAILED"
		if errors.Is(err, errDrainDecisionPending) {
			code = "AGENT_SETTINGS_APPROVAL_PENDING"
		}
		failUnstarted(code)
		return
	}
	if err = m.validateTUISettings(ctx, store, op, false); err != nil {
		failUnstarted("AGENT_SETTINGS_CONFLICT")
		return
	}
	execution, err := m.prepareTUISettingsExecution(ctx, store, rec, op, false)
	if err != nil {
		failUnstarted("AGENT_SETTINGS_CAPABILITIES_UNAVAILABLE")
		return
	}
	pinned, err := store.PinAgentSettingsExecution(ctx, op.SessionID, op.ID, op.Version, execution, m.clock())
	if err != nil {
		failUnstarted("AGENT_SETTINGS_CONFLICT")
		return
	}
	op = pinned
	next, err := store.AdvanceAgentSettingsOperation(ctx, op.SessionID, op.ID, op.Version, domain.AgentSettingsApplyingModel, m.clock())
	if err != nil {
		failUnstarted("AGENT_SETTINGS_CONFLICT")
		return
	}
	op = next
	sessionID, operationID := op.SessionID, op.ID
	quarantine := func(code string) {
		m.setTUISettingsFence(sessionID, operationID)
		current, readErr := store.AgentSettingsOperation(context.Background(), sessionID, operationID)
		if readErr == nil && current.Phase == domain.AgentSettingsPending {
			_, _ = store.FinishAgentSettingsOperationWithCode(context.Background(), sessionID, operationID, current.Version, domain.AgentSettingsRecoveryRequired, domain.AgentSettingsErrorRecovery, code, m.clock())
		}
	}
	if err = m.moveInterfaceTransition(op.ID, domain.SessionInterfaceTransitionSourceStopping, "", ""); err != nil {
		quarantine("AGENT_SETTINGS_STORAGE_FAILED")
		return
	}
	if err = m.stopSourceControllerConclusive(rec); err != nil {
		quarantine("AGENT_SETTINGS_SOURCE_STOP_UNCERTAIN")
		return
	}
	if err = m.moveInterfaceTransition(op.ID, domain.SessionInterfaceTransitionSourceStopped, "", ""); err != nil {
		quarantine("AGENT_SETTINGS_STORAGE_FAILED")
		return
	}
	state := &tuiSettingsLaunch{settings: op.Target, sessionID: op.SessionID, accountID: op.AccountID, nativeID: op.ProviderConversationID, operationID: op.ID, targetGeneration: op.TUI.TargetGeneration, native: &op.Execution.Plan.After, protocol: op.Execution.Plan.Protocol}
	if err = m.validateTUISettings(ctx, store, op, false); err == nil {
		err = m.moveInterfaceTransition(op.ID, domain.SessionInterfaceTransitionTargetStarting, "", "")
	}
	if err == nil {
		err = m.launchTUISettings(ctx, rec, op.TUI.TargetGeneration, state)
	}
	if err == nil {
		err = m.validateTUISettings(ctx, store, op, false)
	}
	if err == nil {
		op, err = store.AdvanceAgentSettingsOperation(ctx, op.SessionID, op.ID, op.Version, domain.AgentSettingsVerifying, m.clock())
	}
	if err == nil {
		op, err = store.AdvanceAgentSettingsOperation(ctx, op.SessionID, op.ID, op.Version, domain.AgentSettingsVerified, m.clock())
	}
	if err == nil {
		err = m.moveInterfaceTransition(op.ID, domain.SessionInterfaceTransitionActivating, "", "")
	}
	if err == nil {
		_, err = store.CommitAgentSettingsOperation(ctx, op.SessionID, op.ID, m.clock())
	}
	if err == nil {
		return
	}
	quarantine("AGENT_SETTINGS_TARGET_FAILED")
	if saved, readErr := store.AgentSettingsOperation(context.Background(), sessionID, operationID); readErr == nil {
		m.rollbackTUISettings(store, saved, state)
	}
}

func (m *Manager) launchTUISettings(ctx context.Context, rec domain.SessionRecord, generation string, state *tuiSettingsLaunch) error {
	ctx, cancel := context.WithTimeout(ctx, interfaceTransitionStepLimit)
	defer cancel()
	ctx = context.WithValue(ctx, tuiSettingsLaunchKey{}, state)
	current, found, err := m.store.GetSession(ctx, rec.ID)
	if err != nil {
		return err
	}
	if !found || current.IsTerminated || current.Metadata.AgentSessionID != state.nativeID {
		return ports.ErrAgentSettingsConflict
	}
	project, err := m.loadProject(ctx, current.ProjectID)
	if err != nil {
		return err
	}
	_, err = m.relaunchSessionWithPolicyAndGeneration(ctx, "reconfigure settings", current, project, workspaceInfo(current), nil, false, true, generation, domain.SessionInterfaceTransitionHistoryStrict, nil)
	return err
}

func (m *Manager) stopTUISettingsTarget(state *tuiSettingsLaunch) bool {
	if !state.attempted || state.noEffects {
		return true
	}
	if state.handle.ID == "" {
		if store, ok := m.store.(tuiSettingsStore); ok && state.operationID != "" {
			_ = store.SetTUIAgentSettingsRuntimeUnknown(context.Background(), state.sessionID, state.operationID, true)
		}
		return false
	}
	return m.stopSourceControllerConclusive(domain.SessionRecord{Mode: domain.SessionModeTUI, Metadata: domain.SessionMetadata{RuntimeHandleID: state.handle.ID}}) == nil
}

func (m *Manager) rollbackTUISettings(store tuiSettingsStore, op domain.AgentSettingsOperation, state *tuiSettingsLaunch) {
	if !m.stopTUISettingsTarget(state) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), interfaceTransitionStepLimit)
	defer cancel()
	current, err := store.SessionAgentSettings(ctx, op.SessionID)
	if err != nil {
		return
	}
	if err = m.validateTUISettings(ctx, store, op, true); err != nil {
		return
	}
	op, err = store.AgentSettingsOperation(ctx, op.SessionID, op.ID)
	if err != nil {
		return
	}
	rec, found, err := m.store.GetSession(ctx, op.SessionID)
	if err != nil || !found {
		return
	}
	execution, err := m.prepareTUISettingsRestoration(ctx, store, rec, op)
	if err != nil {
		return
	}
	recovery := domain.AgentSettingsRecovery{Owner: settingsOwner(current), Revision: current.Revision, Settings: current.Settings, Plan: &execution.Plan, Qualification: &execution.Qualification}
	op, err = store.AdvanceAgentSettingsRecovery(ctx, op.SessionID, op.ID, op.Version, domain.AgentSettingsRollingBack, recovery, m.clock())
	if err != nil {
		return
	}
	rollback := &tuiSettingsLaunch{settings: op.Source, sessionID: op.SessionID, accountID: op.AccountID, nativeID: op.ProviderConversationID, operationID: op.ID, targetGeneration: op.TUI.TargetGeneration, rollback: true, native: &execution.Plan.After, protocol: execution.Plan.Protocol}
	if err = m.launchTUISettings(ctx, rec, op.ID+":rollback", rollback); err != nil {
		m.stopTUISettingsTarget(rollback)
		return
	}
	current, err = store.SessionAgentSettings(ctx, op.SessionID)
	if err != nil {
		return
	}
	if err = m.validateTUISettings(ctx, store, op, true); err != nil {
		return
	}
	recovery = domain.AgentSettingsRecovery{Owner: settingsOwner(current), Revision: current.Revision, Settings: current.Settings, Plan: &execution.Plan, Qualification: &execution.Qualification}
	op, err = store.AdvanceAgentSettingsRecovery(ctx, op.SessionID, op.ID, op.Version, domain.AgentSettingsRollingBack, recovery, m.clock())
	if err != nil {
		return
	}
	op, err = store.AdvanceAgentSettingsRecovery(ctx, op.SessionID, op.ID, op.Version, domain.AgentSettingsRestored, recovery, m.clock())
	if err != nil {
		return
	}
	if _, err = store.FinishAgentSettingsOperationWithCode(ctx, op.SessionID, op.ID, op.Version, domain.AgentSettingsFailed, domain.AgentSettingsErrorProvider, "", m.clock()); err == nil {
		m.clearTUISettingsFence(op.SessionID, op.ID)
	}
}

// CancelTUIAgentSettings never signals a provider. Its CAS settles both claims.
func (m *Manager) CancelTUIAgentSettings(ctx context.Context, id domain.SessionID, operationID string, version int64) (domain.AgentSettingsOperation, error) {
	store, ok := m.store.(tuiSettingsStore)
	if !ok {
		return domain.AgentSettingsOperation{}, ports.ErrAgentSettingsCapabilitiesUnavailable
	}
	op, err := store.FinishAgentSettingsOperationWithCode(ctx, id, operationID, version, domain.AgentSettingsCancelled, domain.AgentSettingsErrorCancelled, "", m.clock())
	if err == nil {
		m.transitionMu.Lock()
		if run := m.transitions[id]; run != nil {
			run.cancel()
		}
		m.transitionMu.Unlock()
	}
	return op, err
}

func (m *Manager) setTUISettingsFence(id domain.SessionID, operationID string) {
	m.agentOpMu.Lock()
	defer m.agentOpMu.Unlock()
	if m.tuiSettingsFences == nil {
		m.tuiSettingsFences = make(map[domain.SessionID]string)
	}
	m.tuiSettingsFences[id] = operationID
}
func (m *Manager) clearTUISettingsFence(id domain.SessionID, operationID string) {
	m.agentOpMu.Lock()
	defer m.agentOpMu.Unlock()
	if m.tuiSettingsFences[id] == operationID {
		delete(m.tuiSettingsFences, id)
	}
}

// Startup reconstructs the input/reaper fence, never a native process. A
// begun handoff may own a target not yet published to sessions; ordinary
// restore's fresh fallback must not run until that obligation is resolved.
func (m *Manager) recoverTUISettingsStartup(ctx context.Context, tr domain.SessionInterfaceTransition) error {
	m.setTUISettingsFence(tr.SessionID, tr.SettingsOperationID)
	defer m.endAgentOperation(tr.SessionID, agentOperationInterfaceRecovery)
	store, ok := m.store.(tuiSettingsStore)
	if !ok {
		return ports.ErrAgentSettingsCapabilitiesUnavailable
	}
	op, err := store.AgentSettingsOperation(ctx, tr.SessionID, tr.SettingsOperationID)
	if err != nil {
		return err
	}
	current, err := store.SessionAgentSettings(ctx, op.SessionID)
	if err != nil {
		return err
	}
	if current.IsTerminated && op.StartedAt != nil {
		return m.settleTerminatedTUISettings(ctx, op.SessionID)
	}
	if op.StartedAt == nil && op.Phase == domain.AgentSettingsPending {
		if _, err := store.FinishAgentSettingsOperationWithCode(ctx, op.SessionID, op.ID, op.Version, domain.AgentSettingsFailed, domain.AgentSettingsErrorStorage, "AGENT_SETTINGS_DAEMON_RESTARTED", m.clock()); err != nil {
			return err
		}
		m.clearTUISettingsFence(op.SessionID, op.ID)
		return nil
	}
	// Every Create is preceded by the durable unknown-outcome marker. Do not
	// infer absence from metadata, or overwrite a proven no-effect receipt.
	if op.Phase == domain.AgentSettingsPending {
		_, err = store.FinishAgentSettingsOperationWithCode(ctx, op.SessionID, op.ID, op.Version, domain.AgentSettingsRecoveryRequired, domain.AgentSettingsErrorRecovery, "AGENT_SETTINGS_DAEMON_RESTARTED", m.clock())
	}
	return err
}

func (m *Manager) prepareTUISettingsExecution(ctx context.Context, store tuiSettingsStore, rec domain.SessionRecord, op domain.AgentSettingsOperation, restoring bool) (domain.AgentSettingsExecution, error) {
	var empty domain.AgentSettingsExecution
	if m.tuiSettingsValidator == nil {
		return empty, ports.ErrAgentSettingsCapabilitiesUnavailable
	}
	current, err := store.SessionAgentSettings(ctx, op.SessionID)
	if err != nil {
		return empty, err
	}
	qualification, err := m.tuiSettingsValidator.ValidateAgentSettings(ctx, op, settingsOwner(current), restoring)
	if err != nil {
		return empty, err
	}
	agent, ok := m.agents.Agent(rec.Harness)
	if !ok {
		return empty, ErrUnknownHarness
	}
	preparer, ok := agent.(ports.AgentTUISettingsPreparer)
	if !ok {
		return empty, ports.ErrAgentSettingsUnsupported
	}
	project, err := m.loadProject(ctx, rec.ProjectID)
	if err != nil {
		return empty, err
	}
	env, err := m.runtimeEnvChecked(rec.ID, rec.ProjectID, rec.IssueID, project.Config.Env, op.AccountID, rec.Harness)
	if err != nil {
		return empty, err
	}
	target := op.Target
	if restoring {
		target = op.Source
	}
	plan, err := preparer.PrepareTUIAgentSettings(ctx, ports.RestoreConfig{Env: env, Session: ports.SessionRef{ID: string(rec.ID), WorkspacePath: rec.Metadata.WorkspacePath, Metadata: map[string]string{ports.MetadataKeyAgentSessionID: op.ProviderConversationID}}, Permissions: rec.Metadata.Permissions, Config: restoredAgentConfig(rec, project.Config)}, op.Source, target)
	if err != nil {
		if ctx.Err() != nil {
			return empty, ctx.Err()
		}
		// Provider probe diagnostics never cross the restoration boundary.
		return empty, ports.ErrAgentSettingsCapabilitiesUnavailable
	}
	again, err := m.tuiSettingsValidator.ValidateAgentSettings(ctx, op, settingsOwner(current), restoring)
	if err != nil {
		return empty, err
	}
	if qualification != again {
		return empty, ports.ErrAgentSettingsCapabilitiesUnavailable
	}
	return domain.AgentSettingsExecution{Owner: settingsOwner(current), Revision: current.Revision, Qualification: qualification, Plan: plan}, nil
}

// prepareTUISettingsRestoration prefers today's qualified baseline. If discovery
// or a provider probe is unavailable, replay only the same conversation's pinned
// native values. Native history and the physical writer barrier remain required.
func (m *Manager) prepareTUISettingsRestoration(ctx context.Context, store tuiSettingsStore, rec domain.SessionRecord, op domain.AgentSettingsOperation) (domain.AgentSettingsExecution, error) {
	var empty domain.AgentSettingsExecution
	if err := m.validateTUISettings(ctx, store, op, true); err != nil {
		return empty, err
	}
	current, err := store.SessionAgentSettings(ctx, op.SessionID)
	if err != nil {
		return empty, err
	}
	if current.Settings.Validate() != nil {
		return empty, ports.ErrAgentSettingsInvalid
	}
	if current.Mode != domain.SessionModeTUI || current.Harness != rec.Harness || current.OwnerGeneration != rec.Metadata.RuntimeLaunchID || current.ProviderConversationID != rec.Metadata.AgentSessionID {
		return empty, ports.ErrAgentSettingsConflict
	}
	execution, err := m.prepareTUISettingsExecution(ctx, store, rec, op, true)
	if err == nil {
		again, readErr := store.SessionAgentSettings(ctx, op.SessionID)
		if readErr != nil {
			return empty, readErr
		}
		if again != current {
			return empty, ports.ErrAgentSettingsConflict
		}
		return execution, nil
	}
	if ctx.Err() != nil {
		return empty, ctx.Err()
	}
	if !errors.Is(err, ports.ErrAgentSettingsCapabilitiesUnavailable) && !errors.Is(err, ports.ErrAgentSettingsUnsupported) && !errors.Is(err, ports.ErrAgentSettingsInvalid) {
		return empty, err
	}
	applied, found, readErr := store.LatestAppliedAgentSettingsOperation(ctx, op.SessionID)
	if readErr != nil {
		return empty, readErr
	}
	pin := op.Execution
	var values domain.AgentSettingsNativeValues
	if found && applied.TUI != nil && applied.Target == current.Settings && applied.BaseRevision+1 == current.Revision && applied.AccountID == current.AccountID && applied.ProviderConversationID == current.ProviderConversationID {
		pin = applied.Execution
		if pin == nil || pin.Plan.Target != current.Settings || pin.Revision != applied.BaseRevision {
			return empty, ports.ErrAgentSettingsCapabilitiesUnavailable
		}
		values = pin.Plan.After
	} else {
		// An interrupted first mutation still has its qualified source preimage.
		if pin == nil || pin.Revision != current.Revision || pin.Plan.Source != current.Settings {
			return empty, ports.ErrAgentSettingsCapabilitiesUnavailable
		}
		values = pin.Plan.Before
	}
	protocol := "codex-native-resume"
	if rec.Harness == domain.HarnessClaudeCode {
		protocol = "claude-native-resume"
	}
	if pin.Owner.AccountID != current.AccountID || pin.Owner.ProviderConversationID != current.ProviderConversationID || pin.Owner.SessionID != current.SessionID || pin.Plan.ProviderConversationID != current.ProviderConversationID || pin.Plan.Protocol != protocol || pin.Qualification.InputFingerprint == "" {
		return empty, ports.ErrAgentSettingsCapabilitiesUnavailable
	}
	again, err := store.SessionAgentSettings(ctx, op.SessionID)
	if err != nil {
		return empty, err
	}
	if again != current {
		return empty, ports.ErrAgentSettingsConflict
	}
	plan := domain.AgentSettingsProviderPlan{Protocol: protocol, ProviderConversationID: current.ProviderConversationID, Source: current.Settings, Target: current.Settings, Before: values, After: values, Permission: string(rec.Metadata.Permissions)}
	qualification := pin.Qualification
	qualification.PolicyVersion = "pinned_restoration"
	return domain.AgentSettingsExecution{Owner: settingsOwner(current), Revision: current.Revision, Plan: plan, Qualification: qualification}, nil
}

// committedTUISettings prefers today's baseline for the durable preference.
// Qualified native wire remains available when discovery or probing cannot run.
func (m *Manager) committedTUISettings(ctx context.Context, rec domain.SessionRecord) (*domain.AgentSettingsProviderPlan, error) {
	store, ok := m.store.(tuiSettingsStore)
	if !ok {
		return nil, nil
	}
	op, found, err := store.LatestAppliedAgentSettingsOperation(ctx, rec.ID)
	if err != nil || !found || op.TUI == nil {
		return nil, err
	}
	current, err := store.SessionAgentSettings(ctx, rec.ID)
	if err != nil {
		return nil, err
	}
	// An account/provider/interface move owns its own launch contract. Never
	// borrow a prior native conversation's settings evidence for that launch.
	if current.AccountID != op.AccountID || current.ProviderConversationID != op.ProviderConversationID {
		return nil, nil
	}
	if current.Settings != op.Target || current.Revision != op.BaseRevision+1 {
		return nil, ports.ErrAgentSettingsConflict
	}
	op.Source, op.Target = current.Settings, current.Settings
	op.BaseRevision = current.Revision
	execution, err := m.prepareTUISettingsRestoration(ctx, store, rec, op)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ports.ErrAgentSettingsCapabilitiesUnavailable
	}
	return &execution.Plan, nil
}

// resumeTUISettingsRecovery is reached only from explicit Resume, after its
// exclusive operation lease. Automatic refresh/wake/restore never use it.
func (m *Manager) resumeTUISettingsRecovery(ctx context.Context, store tuiSettingsStore, tr domain.SessionInterfaceTransition) (RestoreResult, error) {
	recoveryError := func() (RestoreResult, error) {
		return RestoreResult{}, apierr.Conflict("AGENT_SETTINGS_RECOVERY_REQUIRED", "Settings recovery could not be confirmed; inspect the operation before retrying", nil)
	}
	op, err := store.AgentSettingsOperation(ctx, tr.SessionID, tr.SettingsOperationID)
	if err != nil || op.TUI == nil || op.Phase != domain.AgentSettingsRecoveryRequired || op.TUI.UnconfirmedRuntime {
		return recoveryError()
	}
	if err = m.validateTUISettings(ctx, store, op, true); err != nil {
		return recoveryError()
	}
	rec, found, err := m.store.GetSession(ctx, op.SessionID)
	if err != nil || !found || rec.IsTerminated || domain.NormalizeSessionMode(rec.Mode) != domain.SessionModeTUI {
		return recoveryError()
	}
	// Every possible recorded process is stopped before a restoration writer.
	// A generation outside this handoff cannot be adopted or destroyed here.
	generation := rec.Metadata.RuntimeLaunchID
	if generation != op.ControllerGeneration && generation != op.TUI.TargetGeneration && generation != op.ID+":rollback" {
		return recoveryError()
	}
	handles := map[string]string{op.TUI.SourceRuntimeHandle: op.ControllerGeneration}
	if op.TUI.TargetRuntimeHandle != "" {
		handles[op.TUI.TargetRuntimeHandle] = op.TUI.TargetGeneration
	}
	if op.TUI.RollbackRuntimeHandle != "" {
		handles[op.TUI.RollbackRuntimeHandle] = op.ID + ":rollback"
	}
	if rec.Metadata.RuntimeHandleID != "" {
		handles[rec.Metadata.RuntimeHandleID] = generation
	}
	for handle, generation := range handles {
		if handle == "" {
			continue
		}
		proof := m.runtime.ProbeFencedRuntime(ctx, ports.FencedRuntimeRef{Handle: ports.RuntimeHandle{ID: handle}, SessionID: op.SessionID, Generation: generation})
		if proof.Liveness == ports.FencedUnknown {
			return recoveryError()
		}
		if err = m.stopSourceControllerConclusive(domain.SessionRecord{ID: op.SessionID, Mode: domain.SessionModeTUI, Metadata: domain.SessionMetadata{RuntimeHandleID: handle}}); err != nil {
			return recoveryError()
		}
	}
	m.rollbackTUISettings(store, op, &tuiSettingsLaunch{})
	finished, err := store.AgentSettingsOperation(ctx, op.SessionID, op.ID)
	if err != nil || finished.Phase != domain.AgentSettingsFailed || finished.Checkpoint != domain.AgentSettingsRestored {
		return recoveryError()
	}
	restored, found, err := m.store.GetSession(ctx, op.SessionID)
	if err != nil || !found {
		return recoveryError()
	}
	return RestoreResult{Session: restored, Mode: RestoreModeNative}, nil
}

type tuiSettingsTerminationStore interface {
	PendingAgentSettingsOperation(context.Context, domain.SessionID) (domain.AgentSettingsOperation, bool, error)
	AbandonTerminatedTUISettings(context.Context, domain.SessionID, string, int64, bool, time.Time) (domain.AgentSettingsOperation, error)
	HasAbandonedTUISettingsWriter(context.Context, domain.SessionID) (bool, error)
}
type tuiSettingsTerminationKey struct{}
type tuiSettingsTerminationProof struct {
	operationID string
	verified    bool
}

// prepareTUISettingsKill runs under the exclusive Kill lease. Recovery may own
// handles not yet published to sessions; stop all of them, never infer that a
// missing launch receipt proves absence. Unknown writers retain their workspace.
func (m *Manager) prepareTUISettingsKill(ctx context.Context, id domain.SessionID) (context.Context, bool, error) {
	store, ok := m.store.(tuiSettingsTerminationStore)
	if !ok {
		return ctx, false, ErrInterfaceTransitionInProgress
	}
	op, found, err := store.PendingAgentSettingsOperation(ctx, id)
	if err != nil {
		return ctx, false, err
	}
	if !found || op.TUI == nil || op.Phase != domain.AgentSettingsRecoveryRequired {
		return ctx, false, ErrInterfaceTransitionInProgress
	}
	for _, handle := range []string{op.TUI.SourceRuntimeHandle, op.TUI.TargetRuntimeHandle, op.TUI.RollbackRuntimeHandle} {
		if handle != "" {
			if err := m.stopSourceControllerConclusive(domain.SessionRecord{ID: id, Mode: domain.SessionModeTUI, Metadata: domain.SessionMetadata{RuntimeHandleID: handle}}); err != nil {
				return ctx, false, err
			}
		}
	}
	proof := tuiSettingsTerminationProof{operationID: op.ID, verified: !op.TUI.UnconfirmedRuntime}
	return context.WithValue(ctx, tuiSettingsTerminationKey{}, proof), op.TUI.UnconfirmedRuntime, nil
}

func (m *Manager) settleTerminatedTUISettings(ctx context.Context, id domain.SessionID) error {
	store, ok := m.store.(tuiSettingsTerminationStore)
	if !ok {
		return nil
	}
	op, found, err := store.PendingAgentSettingsOperation(ctx, id)
	if err != nil || !found || op.TUI == nil || op.StartedAt == nil {
		return err
	}
	proof, _ := ctx.Value(tuiSettingsTerminationKey{}).(tuiSettingsTerminationProof)
	_, err = store.AbandonTerminatedTUISettings(ctx, id, op.ID, op.Version, proof.operationID == op.ID && proof.verified, m.clock())
	if err == nil {
		m.clearTUISettingsFence(id, op.ID)
	}
	return err
}

func (m *Manager) abandonedTUISettingsWriter(ctx context.Context, id domain.SessionID) (bool, error) {
	if store, ok := m.store.(tuiSettingsTerminationStore); ok {
		return store.HasAbandonedTUISettingsWriter(ctx, id)
	}
	return false, nil
}
