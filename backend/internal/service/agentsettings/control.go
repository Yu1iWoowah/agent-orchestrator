package agentsettings

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/reqid"
)

// Change preserves patch presence and the caller's CAS. Generated IDs,
// request IDs and credentials do not form its idempotency identity.
type Change struct {
	ExpectedRevision *int64                    `json:"expectedRevision" nullable:"false" minimum:"0"`
	Settings         domain.AgentSettingsPatch `json:"settings"`
	// Restart explicitly consents to a same-native-conversation TUI restart.
	ApplyMode domain.AgentSettingsApplyMode `json:"applyMode,omitempty" enum:"after-turn,restart"`
	// StopTurn permits interrupting a running turn, only with restart consent.
	// It never authorizes answering a pending approval.
	StopTurn       bool   `json:"stopTurn,omitempty"`
	IdempotencyKey string `json:"-"`
	RequestID      string `json:"-"`
}

// ControlStore supplies durable ownership and settings operation evidence.
type ControlStore interface {
	AuthorityStore
	SessionAgentSettings(context.Context, domain.SessionID) (domain.SessionAgentSettings, error)
	AgentSettingsOperationByKey(context.Context, domain.SessionID, string) (domain.AgentSettingsOperation, bool, error)
	AgentSettingsOperation(context.Context, domain.SessionID, string) (domain.AgentSettingsOperation, error)
	PendingAgentSettingsOperation(context.Context, domain.SessionID) (domain.AgentSettingsOperation, bool, error)
}

// SettingsRuntime admits intent to the session dispatch scheduler.
type SettingsRuntime interface {
	QueueAgentSettings(context.Context, domain.AgentSettingsOperation) (domain.AgentSettingsOperation, error)
	CancelAgentSettings(context.Context, domain.SessionID, string, int64) (domain.AgentSettingsOperation, error)
}

// TUISettingsRuntime owns the native-process handoff. Restart consent never
// routes a terminal through the Chat controller or a cross-provider switch.
type TUISettingsRuntime interface {
	QueueTUIAgentSettings(context.Context, domain.AgentSettingsOperation) (domain.AgentSettingsOperation, error)
	CancelTUIAgentSettings(context.Context, domain.SessionID, string, int64) (domain.AgentSettingsOperation, error)
}

// Control is the shared authorization/CAS/provider boundary for all mutation
// transports. It owns no provider setters and cannot manufacture a runtime.
type Control struct {
	core       *Service
	store      ControlStore
	authorizer *Authorizer
	runtime    SettingsRuntime
	tui        TUISettingsRuntime
	now        func() time.Time
}

// NewControl joins authorization, provider validation and journalled scheduling.
func NewControl(core *Service, store ControlStore, authorizer *Authorizer, runtime SettingsRuntime, now func() time.Time) *Control {
	if now == nil {
		now = time.Now
	}
	c := &Control{core: core, store: store, authorizer: authorizer, runtime: runtime, now: now}
	c.tui, _ = runtime.(TUISettingsRuntime)
	return c
}

// SetTUIRuntime wires the terminal coordinator before the API opens.
func (c *Control) SetTUIRuntime(runtime TUISettingsRuntime) { c.tui = runtime }

// ChangeAgentSettings adapts pre-CAS manual menus into the same scheduler. It
// never changes project defaults, writes session metadata separately or claims
// that an identity-free local caller was a verified human.
func (c *Control) ChangeAgentSettings(ctx context.Context, id domain.SessionID, patch domain.AgentSettingsPatch, queue ports.ChatAgentSettingsQueue) (domain.AgentSettingsOperation, error) {
	caller := ports.AgentSettingsCallerFromContext(ctx)
	key := uuid.NewString()
	for attempt := 0; attempt < 8; attempt++ {
		current, err := c.store.SessionAgentSettings(ctx, id)
		if err != nil {
			return domain.AgentSettingsOperation{}, settingsError(err)
		}
		var previous *domain.AgentSettingsOperation
		enqueue := queue.QueueAgentSettings
		if caller == (Caller{}) {
			pending, found, err := c.store.PendingAgentSettingsOperation(ctx, id)
			if err != nil {
				return domain.AgentSettingsOperation{}, settingsError(err)
			}
			if found {
				if !pending.CanSupersedeLocally() {
					return domain.AgentSettingsOperation{}, settingsError(ports.ErrAgentSettingsConflict)
				}
				previous = &pending
				enqueue = func(ctx context.Context, next domain.AgentSettingsOperation) (domain.AgentSettingsOperation, error) {
					return queue.SupersedeQueuedAgentSettings(ctx, pending, next)
				}
			}
		}
		result, err := c.change(ctx, caller, id, Change{ExpectedRevision: &current.Revision, Settings: patch, IdempotencyKey: key, RequestID: reqid.FromContext(ctx)}, enqueue, previous)
		var structured *apierr.Error
		if caller != (Caller{}) || !errors.As(err, &structured) || structured.Code != "AGENT_SETTINGS_CONFLICT" {
			return result, settingsError(err)
		}
	}
	return domain.AgentSettingsOperation{}, settingsError(ports.ErrAgentSettingsConflict)
}

func (c *Control) target(ctx context.Context, caller Caller, id domain.SessionID) (domain.SessionRecord, Authorization, error) {
	rec, ok, err := c.store.GetSession(ctx, id)
	if err != nil {
		return rec, Authorization{}, err
	}
	if !ok {
		return rec, Authorization{}, apierr.NotFound("AGENT_SETTINGS_NOT_FOUND", "Session settings not found")
	}
	accepted, err := c.authorizer.Authorize(ctx, caller, rec)
	return rec, accepted, err
}

// Change authorizes and validates a patch before admitting it with revision CAS.
func (c *Control) Change(ctx context.Context, caller Caller, id domain.SessionID, in Change) (domain.AgentSettingsOperation, error) {
	var enqueue func(context.Context, domain.AgentSettingsOperation) (domain.AgentSettingsOperation, error)
	if c.runtime != nil {
		enqueue = c.runtime.QueueAgentSettings
	}
	op, err := c.change(ctx, caller, id, in, enqueue, nil)
	return op, settingsError(err)
}

func (c *Control) change(ctx context.Context, caller Caller, id domain.SessionID, in Change, enqueue func(context.Context, domain.AgentSettingsOperation) (domain.AgentSettingsOperation, error), previous *domain.AgentSettingsOperation) (domain.AgentSettingsOperation, error) {
	var empty domain.AgentSettingsOperation
	if in.ApplyMode == "" {
		in.ApplyMode = domain.AgentSettingsAfterTurn
	}
	if in.StopTurn && in.ApplyMode != domain.AgentSettingsRestart {
		return empty, apierr.Invalid("AGENT_SETTINGS_INVALID", "stopTurn requires restart", nil)
	}
	if in.ExpectedRevision == nil || *in.ExpectedRevision < 0 || !validGrantText(in.IdempotencyKey) || len(in.RequestID) > 256 || strings.ContainsAny(in.RequestID, "\r\n\x00") || in.Settings == (domain.AgentSettingsPatch{}) || (in.ApplyMode != domain.AgentSettingsAfterTurn && in.ApplyMode != domain.AgentSettingsRestart) {
		return empty, apierr.Invalid("AGENT_SETTINGS_INVALID", "A settings patch, expectedRevision and idempotency key are required", nil)
	}
	rec, accepted, err := c.target(ctx, caller, id)
	if err != nil {
		return empty, err
	}
	raw, err := json.Marshal(struct {
		SessionID domain.SessionID
		Revision  int64
		Patch     domain.AgentSettingsPatch
		Mode      domain.AgentSettingsApplyMode
		Actor     domain.AgentSettingsActor
		StopTurn  bool `json:"StopTurn,omitempty"`
	}{id, *in.ExpectedRevision, in.Settings, in.ApplyMode, accepted.Actor, in.StopTurn})
	if err != nil {
		return empty, settingsError(ports.ErrAgentSettingsInvalid)
	}
	digest := sha256.Sum256(raw)
	fingerprint := hex.EncodeToString(digest[:])
	prior, found, err := c.store.AgentSettingsOperationByKey(ctx, id, in.IdempotencyKey)
	if err != nil {
		return empty, err
	}
	if found {
		if prior.RequestFingerprint != fingerprint {
			return empty, settingsError(ports.ErrAgentSettingsConflict)
		}
		return prior, nil
	}
	if rec.IsTerminated {
		return empty, apierr.Conflict("AGENT_SETTINGS_CONFLICT", "The session is terminated", nil)
	}
	if domain.NormalizeSessionMode(rec.Mode) == domain.SessionModeTUI {
		if in.ApplyMode != domain.AgentSettingsRestart {
			return empty, apierr.Conflict("AGENT_SETTINGS_RESTART_REQUIRED", "This session requires an explicit restart workflow", nil)
		}
		if c.tui == nil {
			return empty, settingsError(ports.ErrAgentSettingsCapabilitiesUnavailable)
		}
		enqueue = c.tui.QueueTUIAgentSettings
	} else if in.ApplyMode != domain.AgentSettingsAfterTurn {
		return empty, apierr.Conflict("AGENT_SETTINGS_RESTART_REQUIRED", "This session requires an explicit restart workflow", nil)
	}
	current, err := c.store.SessionAgentSettings(ctx, id)
	if err != nil {
		return empty, settingsError(err)
	}
	if current.Revision != *in.ExpectedRevision {
		return c.acceptedRetry(ctx, id, in.IdempotencyKey, fingerprint)
	}
	base := current.Settings
	if previous != nil {
		if accepted.Actor.Kind != domain.AgentSettingsActorLocal || !previous.CanSupersedeLocally() || previous.BaseRevision != current.Revision || previous.Source != current.Settings {
			return empty, settingsError(ports.ErrAgentSettingsConflict)
		}
		base = previous.Target
	}
	target, err := base.Apply(in.Settings)
	if err != nil {
		return empty, settingsError(ports.ErrAgentSettingsInvalid)
	}
	selection := target
	if rec.Harness == domain.HarnessClaudeCode && in.Settings.FastMode == nil {
		selection.FastMode = domain.AgentFastDefault
	}
	validated, err := c.core.ValidateSpawn(ctx, id, selection, in.Settings.Model != nil && in.Settings.Effort == nil && target.Model != base.Model)
	if err != nil {
		return empty, settingsError(err)
	}
	target.Effort = validated.Effort
	if _, err := c.authorizer.CheckSelection(ctx, accepted, rec, target); err != nil {
		return empty, err
	}
	if _, err := c.selectionProof(ctx, id, target, rec.Harness == domain.HarnessClaudeCode && in.Settings.FastMode == nil); err != nil {
		return empty, settingsError(err)
	}
	op := domain.AgentSettingsOperation{ID: uuid.NewString(), SessionID: id, IdempotencyKey: in.IdempotencyKey, BaseRevision: current.Revision, Source: current.Settings, Target: target, AccountID: current.AccountID, ControllerGeneration: current.OwnerGeneration, ProviderConversationID: current.ProviderConversationID, BranchID: current.BranchID, Actor: accepted.Actor, Authority: accepted.Authority, RequestFingerprint: fingerprint, CreatedAt: c.now(), RequestID: in.RequestID, ApplyMode: in.ApplyMode}
	op.StopTurn = in.StopTurn
	if enqueue == nil {
		return empty, settingsError(ports.ErrAgentSettingsCapabilitiesUnavailable)
	}
	result, err := enqueue(ctx, op)
	if errors.Is(err, ports.ErrAgentSettingsConflict) {
		return c.acceptedRetry(ctx, id, in.IdempotencyKey, fingerprint)
	}
	return result, settingsError(err)
}

func (c *Control) acceptedRetry(ctx context.Context, id domain.SessionID, key, fingerprint string) (domain.AgentSettingsOperation, error) {
	original, found, err := c.store.AgentSettingsOperationByKey(ctx, id, key)
	if err != nil {
		return domain.AgentSettingsOperation{}, err
	}
	if found && original.RequestFingerprint == fingerprint {
		return original, nil
	}
	return domain.AgentSettingsOperation{}, settingsError(ports.ErrAgentSettingsConflict)
}

// Operation reads durable progress after checking the current caller scope.
func (c *Control) Operation(ctx context.Context, caller Caller, id domain.SessionID, operationID string) (domain.AgentSettingsOperation, error) {
	if _, _, err := c.target(ctx, caller, id); err != nil {
		return domain.AgentSettingsOperation{}, settingsError(err)
	}
	op, err := c.store.AgentSettingsOperation(ctx, id, operationID)
	return op, settingsError(err)
}

// ReadForCaller qualifies access before reading preferences and observations.
func (c *Control) ReadForCaller(ctx context.Context, caller Caller, id domain.SessionID) (Snapshot, error) {
	if _, _, err := c.target(ctx, caller, id); err != nil {
		return Snapshot{}, settingsError(err)
	}
	result, err := c.core.Read(ctx, id)
	return result, settingsError(err)
}

// Cancel terminates an unstarted intent using the caller scope and version CAS.
func (c *Control) Cancel(ctx context.Context, caller Caller, id domain.SessionID, operationID string, version int64) (domain.AgentSettingsOperation, error) {
	_, accepted, err := c.target(ctx, caller, id)
	if err != nil {
		return domain.AgentSettingsOperation{}, settingsError(err)
	}
	operation, err := c.store.AgentSettingsOperation(ctx, id, operationID)
	if err != nil {
		return domain.AgentSettingsOperation{}, settingsError(err)
	}
	if accepted.Actor.Kind == domain.AgentSettingsActorOrchestrator {
		if operation.Actor != accepted.Actor {
			return domain.AgentSettingsOperation{}, apierr.Forbidden("AGENT_SETTINGS_CANCEL_DENIED", "Managed callers may only cancel their own settings operation")
		}
	}
	if operation.ApplyMode == domain.AgentSettingsRestart {
		if c.tui == nil {
			return domain.AgentSettingsOperation{}, settingsError(ports.ErrAgentSettingsCapabilitiesUnavailable)
		}
		op, err := c.tui.CancelTUIAgentSettings(ctx, id, operationID, version)
		return op, settingsError(err)
	}
	if c.runtime == nil {
		return domain.AgentSettingsOperation{}, settingsError(ports.ErrAgentSettingsCapabilitiesUnavailable)
	}
	op, err := c.runtime.CancelAgentSettings(ctx, id, operationID, version)
	return op, settingsError(err)
}

func settingsError(err error) error {
	if err == nil {
		return nil
	}
	var structured *apierr.Error
	if errors.As(err, &structured) {
		return structured
	}
	switch {
	case errors.Is(err, ports.ErrAgentSettingsNativeHistoryUnavailable):
		return apierr.Conflict("AGENT_SETTINGS_NATIVE_HISTORY_UNAVAILABLE", "The original native conversation cannot be resumed; fresh fallback is disabled", nil)
	case errors.Is(err, context.Canceled):
		return apierr.Conflict("AGENT_SETTINGS_REQUEST_CANCELLED", "The settings request was cancelled; query its operation before retrying", nil)
	case errors.Is(err, context.DeadlineExceeded):
		return apierr.Unavailable("AGENT_SETTINGS_REQUEST_TIMEOUT", "The settings request deadline expired; query its operation before retrying")
	case errors.Is(err, ports.ErrAgentSettingsConflict):
		return apierr.Conflict("AGENT_SETTINGS_CONFLICT", "Settings changed or another operation is active", nil)
	case errors.Is(err, ports.ErrAgentSettingsNotFound):
		return apierr.NotFound("AGENT_SETTINGS_NOT_FOUND", "Settings operation not found")
	case errors.Is(err, ports.ErrAgentSettingsInvalid):
		return apierr.Invalid("AGENT_SETTINGS_INVALID", "The requested settings are invalid", nil)
	case errors.Is(err, ports.ErrAgentSettingsUnsupported):
		return apierr.Invalid("AGENT_SETTINGS_UNSUPPORTED", "The provider does not support this selection", nil)
	case errors.Is(err, ports.ErrAgentSettingsCapabilitiesUnavailable):
		return apierr.Unavailable("AGENT_SETTINGS_CAPABILITIES_UNAVAILABLE", "Fresh settings capabilities are unavailable")
	default:
		// Unknown storage/provider failures must not carry native diagnostics
		// into the HTTP access log or capture pipeline.
		return apierr.New(apierr.KindInternal, "AGENT_SETTINGS_FAILED", "Agent settings could not be applied", nil)
	}
}

// selectionProof requires fresh discovery even for an all-default reset.
// Claude retains dormant fast intent while its live model omits the option;
// a newly explicit fast change still requires advertised support.
func (c *Control) selectionProof(ctx context.Context, id domain.SessionID, settings domain.AgentSettings, dormantFast bool) (string, error) {
	snapshot, err := c.core.readCapabilities(ctx, id)
	if err != nil {
		return "", err
	}
	if snapshot.Capabilities.State != CapabilitySupported || c.core.catalogInputs == nil || snapshot.catalogFingerprint == "" {
		return "", ports.ErrAgentSettingsCapabilitiesUnavailable
	}
	current, err := c.core.catalogInputs.CatalogFingerprint(ctx, string(snapshot.Harness), ports.AccountModelCatalogScope(snapshot.AccountID))
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil || current == "" || current != snapshot.catalogFingerprint {
		return "", ports.ErrAgentSettingsCapabilitiesUnavailable
	}
	selection := settings
	if dormantFast {
		selection.FastMode = domain.AgentFastDefault
	}
	if selection.Model == "" && selection.Effort == "" && selection.FastMode == domain.AgentFastDefault {
		for _, m := range snapshot.Capabilities.Models {
			if m.IsDefault {
				selection.Model = domain.AgentModelSelection(m.Model)
				break
			}
		}
		if selection.Model == "" {
			return "", ports.ErrAgentSettingsCapabilitiesUnavailable
		}
	}
	if _, err := c.core.ValidateSpawn(ctx, id, selection, false); err != nil {
		return "", err
	}
	return current, nil
}

// ValidateAgentSettings is invoked before preparation and every action. Cold
// reconstruction restores committed durable preference; it is not a new grant
// exercise and must not depend on an expired historical actor's authority.
func (c *Control) ValidateAgentSettings(ctx context.Context, op domain.AgentSettingsOperation, owner domain.AgentSettingsOwner, restoring bool) (domain.AgentSettingsQualification, error) {
	rec, ok, err := c.store.GetSession(ctx, op.SessionID)
	if err != nil {
		return domain.AgentSettingsQualification{}, err
	}
	// Ordinary Restore validates an already committed TUI preference while
	// its row is still terminated. This never admits a new mutation.
	restoringCommittedTUI := restoring && rec.Mode == domain.SessionModeTUI && op.Phase == domain.AgentSettingsApplied
	if !ok || rec.IsTerminated && !restoringCommittedTUI {
		return domain.AgentSettingsQualification{}, ports.ErrAgentSettingsConflict
	}
	current, err := c.store.SessionAgentSettings(ctx, op.SessionID)
	if err != nil {
		return domain.AgentSettingsQualification{}, err
	}
	if owner != (domain.AgentSettingsOwner{SessionID: current.SessionID, AccountID: current.AccountID, ControllerGeneration: current.OwnerGeneration, ProviderConversationID: current.ProviderConversationID, BranchID: current.BranchID}) {
		return domain.AgentSettingsQualification{}, ports.ErrAgentSettingsConflict
	}
	policy := "durable_restoration"
	if !restoring {
		policy, err = c.authorizer.CheckSelection(ctx, Authorization{Actor: op.Actor, Authority: op.Authority}, rec, op.Target)
		if err != nil {
			return domain.AgentSettingsQualification{}, err
		}
	}
	selection := op.Target
	if restoring {
		selection = current.Settings
	}
	fingerprint, err := c.selectionProof(ctx, op.SessionID, selection, rec.Harness == domain.HarnessClaudeCode)
	return domain.AgentSettingsQualification{InputFingerprint: fingerprint, PolicyVersion: policy}, err
}
