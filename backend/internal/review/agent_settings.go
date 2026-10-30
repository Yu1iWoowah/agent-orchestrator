package review

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// ReviewerSettingsChange carries a stable review owner and revision. Permission
// modes and account selection are deliberately absent from this operation.
type ReviewerSettingsChange struct {
	BaseRevision   int64                     `json:"baseRevision"`
	Patch          domain.AgentSettingsPatch `json:"patch"`
	IdempotencyKey string                    `json:"idempotencyKey"`
	RequestID      string                    `json:"-"`
}

// SetReviewerSettingsAuthorizer is wired before the daemon admits requests.
// Without an authorizer, reviewer mutation stays disabled.
func (e *Engine) SetReviewerSettingsAuthorizer(authorizer ports.ReviewerSettingsAuthorizer) {
	e.settingsAuthorizer = authorizer
}

// ReadReviewerAgentSettingsForCaller checks transport authority separately
// from the internal owner read used during pass admission and recovery.
func (e *Engine) ReadReviewerAgentSettingsForCaller(ctx context.Context, id string) (domain.ReviewerAgentSettingsState, error) {
	state, err := e.ReviewerAgentSettings(ctx, id)
	if err != nil {
		return state, err
	}
	if e.settingsAuthorizer == nil {
		return state, ports.ErrAgentSettingsUnsupported
	}
	worker, found, err := e.sessions.GetSession(ctx, state.Review.SessionID)
	if err != nil {
		return state, err
	}
	if !found {
		return state, ports.ErrAgentSettingsNotFound
	}
	selection := domain.AgentSettings{FastMode: domain.AgentFastDefault}
	if state.Settings != nil {
		selection = *state.Settings
	}
	_, err = e.settingsAuthorizer.AuthorizeReviewerSelection(ctx, worker, state.Review, selection)
	return state, err
}

func reviewerSelectionConfig(config domain.AgentConfig, settings domain.AgentSettings) domain.AgentConfig {
	config.Model, config.Effort, config.FastMode = string(settings.Model), string(settings.Effort), settings.FastMode
	config.Launch.FastMode, config.Launch.FastModeOff = settings.FastMode == domain.AgentFastOn, settings.FastMode == domain.AgentFastOff
	return config
}

func reviewerSelectionSettings(config domain.AgentConfig) domain.AgentSettings {
	fast := config.FastMode
	if fast == "" {
		fast = domain.AgentFastDefault
	}
	return domain.AgentSettings{Model: domain.AgentModelSelection(config.Model), Effort: domain.AgentEffortSelection(config.Effort), FastMode: fast}
}

func (e *Engine) tryLockWorker(id domain.SessionID) (func(), bool) {
	e.triggerMu.Lock()
	mu := e.triggerLocks[id]
	if mu == nil {
		mu = &sync.Mutex{}
		e.triggerLocks[id] = mu
	}
	e.triggerMu.Unlock()
	if !mu.TryLock() {
		return nil, false
	}
	return mu.Unlock, true
}

// ReviewerAgentSettings returns reviewer-owned preference, not worker defaults
// or a claim about the current pass's provider-effective configuration.
func (e *Engine) ReviewerAgentSettings(ctx context.Context, reviewID string) (domain.ReviewerAgentSettingsState, error) {
	s, ok := e.store.(ports.ReviewerAgentSettingsStore)
	if !ok {
		return domain.ReviewerAgentSettingsState{}, ports.ErrAgentSettingsUnsupported
	}
	return s.ReviewerAgentSettings(ctx, reviewID)
}

// ChangeReviewerAgentSettings pins the next pass without stopping a live
// reviewer or modifying its fixed account/permission rules.
func (e *Engine) ChangeReviewerAgentSettings(ctx context.Context, reviewID string, req ReviewerSettingsChange) (domain.ReviewerAgentSettingsOperation, error) {
	return e.changeReviewerAgentSettings(ctx, reviewID, req, false)
}

func (e *Engine) changeReviewerAgentSettings(ctx context.Context, reviewID string, req ReviewerSettingsChange, workerLocked bool) (domain.ReviewerAgentSettingsOperation, error) {
	var result domain.ReviewerAgentSettingsOperation
	if req.BaseRevision < 0 || req.IdempotencyKey == "" || len(req.IdempotencyKey) > 256 || strings.ContainsAny(req.IdempotencyKey, "\r\n") || req.Patch == (domain.AgentSettingsPatch{}) {
		return result, ports.ErrAgentSettingsInvalid
	}
	s, ok := e.store.(ports.ReviewerAgentSettingsStore)
	if !ok || e.settingsAuthorizer == nil || e.models == nil {
		return result, ports.ErrAgentSettingsUnsupported
	}
	state, err := s.ReviewerAgentSettings(ctx, reviewID)
	if err != nil {
		return result, err
	}
	if !workerLocked {
		unlock, ok := e.tryLockWorker(state.Review.SessionID)
		if !ok {
			return result, ports.ErrAgentSettingsConflict
		}
		defer unlock()
	}
	state, err = s.ReviewerAgentSettings(ctx, reviewID)
	if err != nil {
		return result, err
	}
	worker, found, err := e.sessions.GetSession(ctx, state.Review.SessionID)
	if err != nil {
		return result, err
	}
	if !found || worker.IsTerminated || state.Review.IsArchived {
		return result, ports.ErrAgentSettingsConflict
	}
	if state.Settings == nil {
		return result, ports.ErrAgentSettingsCapabilitiesUnavailable
	}
	if state.Owner.AccountID == "" || state.Owner.Generation == "" || state.Owner.NativeID == "" {
		return result, ports.ErrAgentSettingsCapabilitiesUnavailable
	}
	if req.Patch.FastMode != nil && !domain.AgentHarness(state.Review.Harness).SupportsFastMode() {
		return result, ports.ErrAgentSettingsUnsupported
	}
	target, err := state.Settings.Apply(req.Patch)
	if err != nil {
		return result, ports.ErrAgentSettingsInvalid
	}
	if req.Patch.Model != nil && *req.Patch.Model != state.Settings.Model && req.Patch.Effort == nil {
		target.Effort = ""
	}
	accepted, err := e.settingsAuthorizer.AuthorizeReviewerSelection(ctx, worker, state.Review, target)
	if err != nil {
		return result, err
	}
	raw, err := json.Marshal(struct {
		Revision int64
		Patch    domain.AgentSettingsPatch
	}{req.BaseRevision, req.Patch})
	if err != nil {
		return result, ports.ErrAgentSettingsInvalid
	}
	digest := sha256.Sum256(raw)
	fingerprint := hex.EncodeToString(digest[:])
	if prior, found, err := s.ReviewerAgentSettingsOperationByKey(ctx, reviewID, req.IdempotencyKey); err != nil {
		return result, err
	} else if found {
		if prior.RequestFingerprint != fingerprint || prior.Actor != accepted.Actor {
			return result, ports.ErrAgentSettingsConflict
		}
		return prior, nil
	}
	if state.HasRunningPass || state.Revision != req.BaseRevision {
		return result, ports.ErrAgentSettingsConflict
	}
	// Read the existing binding directly: a mutation cannot select an account.
	accounts, ok := e.store.(ports.ReviewerAgentAccountStore)
	provider, supported := domain.AgentAccountProviderFor(domain.AgentHarness(state.Review.Harness))
	if !ok || !supported || state.Owner.AccountID == "" {
		return result, ports.ErrAgentSettingsCapabilitiesUnavailable
	}
	account, bound, err := accounts.GetReviewerAgentAccount(ctx, reviewID, provider)
	if err != nil {
		return result, err
	}
	if !bound || account.AccountID != state.Owner.AccountID {
		return result, ports.ErrAgentSettingsConflict
	}
	ctx = ports.WithAgentAccount(ctx, account)
	catalogFingerprint, err := e.reviewerModelProof(ctx, state.Review.Harness, reviewerSelectionConfig(domain.AgentConfig{}, target), true)
	if err != nil {
		return result, err
	}
	result = domain.ReviewerAgentSettingsOperation{ID: e.newID(), Owner: state.Owner, BaseRevision: req.BaseRevision, Source: state.Settings, Target: target, Actor: accepted.Actor, Authority: accepted.Authority, PolicyRevision: accepted.PolicyRevision, CatalogFingerprint: catalogFingerprint, RequestID: req.RequestID, IdempotencyKey: req.IdempotencyKey, RequestFingerprint: fingerprint, CreatedAt: e.clock()}
	return s.CommitReviewerAgentSettings(ctx, result)
}

func reviewerPassSelectionChanged(runs []domain.ReviewRun, reviewID string, selection domain.AgentSettings) bool {
	for _, run := range runs {
		if run.ReviewID == reviewID && run.AgentSettings != nil {
			return *run.AgentSettings != selection
		}
	}
	return false
}

// A recovering pass owns its admitted snapshot. Idle reconstruction may use
// the next-pass preference; neither path consults today's project defaults.
func (e *Engine) reviewerRestoreSelection(ctx context.Context, review domain.Review, runs []domain.ReviewRun, fallback domain.AgentConfig) (domain.AgentConfig, *domain.AgentSettings, error) {
	for _, run := range runs {
		if run.ReviewID == review.ID && run.Status == domain.ReviewRunRunning && run.AgentSettings != nil {
			return reviewerSelectionConfig(fallback, *run.AgentSettings), run.AgentSettings, nil
		}
	}
	if s, ok := e.store.(ports.ReviewerAgentSettingsStore); ok {
		state, err := s.ReviewerAgentSettings(ctx, review.ID)
		if err != nil {
			return fallback, nil, err
		}
		if state.Settings != nil {
			return reviewerSelectionConfig(fallback, *state.Settings), state.Settings, nil
		}
	}
	return fallback, nil, nil
}
