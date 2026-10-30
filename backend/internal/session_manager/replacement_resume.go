package sessionmanager

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type replacementNativeResumeKey struct{}

// ResumeReplacementSuccessor reopens only the strongly stopped native thread.
// Unknown caller/launch gaps require recovery, never another fresh controller.
func (m *Manager) ResumeReplacementSuccessor(ctx context.Context, expected domain.OrchestratorReplacement, allowNew bool) (_ ports.ReplacementResumeObservation, resultErr error) {
	var zero ports.ReplacementResumeObservation
	launcherEntered := false
	defer func() {
		if allowNew && !launcherEntered && resultErr != nil {
			resultErr = fmt.Errorf("%w: %w", ports.ErrChatResumeNotLaunched, resultErr)
		}
	}()
	ctx, release, err := m.acquireOrchestratorGate(ctx, expected.ProjectID)
	if err != nil {
		return zero, err
	}
	defer release()
	reader, ok := m.store.(replacementAdmissionReader)
	if !ok {
		return zero, domain.ErrReplacementStateInvalid
	}
	op, found, err := reader.GetActiveOrchestratorReplacement(ctx, expected.ProjectID)
	if err != nil {
		return zero, err
	}
	if !found || op.ID != expected.ID || op.Revision != expected.Revision || op.Phase != domain.ReplacementVerifyingResume || op.SourceRetiredAt == nil || op.SuccessorSessionID == "" || op.SuccessorSessionID != expected.SuccessorSessionID {
		return zero, domain.ErrReplacementStateInvalid
	}
	if op.DeadlineAt.IsZero() || !m.clock().Before(op.DeadlineAt) {
		return zero, &ports.NativeTranscriptError{Code: "ORCHESTRATOR_HANDOFF_RESUME_TIMEOUT", Reason: "Native resume intent expired before launch"}
	}
	ctx, cancel := context.WithDeadline(ctx, op.DeadlineAt)
	defer cancel()
	p := op.ResumeEvidence
	if p == nil || p.StartedAt.IsZero() || !p.StopConfirmed || p.StopReceipt == nil || p.ResumeIntentAt == nil || p.ResumeIntentAt.IsZero() || p.ResumeGeneration == "" || p.ResumeGeneration == p.BeforeOwner.ControllerGeneration || p.ResumedAt != nil || op.Bootstrap == nil || op.Bootstrap.SettledAt == nil {
		return zero, domain.ErrReplacementStateInvalid
	}
	if !allowNew {
		return zero, ports.ErrChatRecoveryInconclusive
	}
	rec, found, err := m.store.GetSession(ctx, op.SuccessorSessionID)
	if err != nil {
		return zero, err
	}
	if !found || rec.ProjectID != op.ProjectID || rec.Kind != domain.KindOrchestrator || rec.Harness != domain.HarnessClaudeCode || domain.NormalizeSessionMode(rec.Mode) != domain.SessionModeChat || rec.ClientRequestID != op.SpawnRequestID || rec.ClientRequestHash != op.SpawnRequestHash || rec.ProvisionState != domain.SessionProvisionReady || rec.ControllerOwner() != p.BeforeOwner || rec.Metadata.BrowserCapabilityVerifier != p.BeforeCapabilityVerifier || p.BeforeNativeID == "" || p.BeforeNativeID != rec.Metadata.ProviderConversationID || !p.StopReceipt.MatchesSource(rec) {
		return zero, domain.ErrReplacementSourceChanged
	}
	initial := op.SuccessorController
	if initial == nil || p.BeforeOwner != initial.Owner || p.BeforeCapabilityVerifier != initial.CapabilityVerifier || p.StopReceipt.Owner != p.BeforeOwner || p.StopReceipt.NativeID != p.BeforeNativeID || p.StopReceipt.CapabilityVerifier != p.BeforeCapabilityVerifier {
		return zero, domain.ErrReplacementStateInvalid
	}
	if m.chat == nil {
		return zero, ports.ErrChatUnsupported
	}
	if m.chat.HasLiveChatController(rec.ID) {
		return zero, ports.ErrChatRecoveryInconclusive
	}
	source, found, err := m.store.GetSession(ctx, op.SourceSessionID)
	if err != nil {
		return zero, err
	}
	retiredOwner := op.SourceOwner
	retiredOwner.IsTerminated = true
	if !found || source.ProjectID != op.ProjectID || source.Kind != domain.KindOrchestrator || source.ControllerOwner() != retiredOwner || source.Metadata.BrowserCapabilityVerifier != op.SourceCapabilityVerifier {
		return zero, domain.ErrReplacementSourceChanged
	}
	project, err := m.loadProject(ctx, op.ProjectID)
	if err != nil {
		return zero, err
	}
	if !project.ArchivedAt.IsZero() {
		return zero, domain.ErrReplacementConfigChanged
	}
	fingerprint, err := domain.ReplacementProjectFingerprint(project)
	if err != nil {
		return zero, err
	}
	if fingerprint != op.ProjectFingerprint || rec.Metadata.Model != op.TargetAgentConfig.Model || rec.Metadata.Effort != op.TargetAgentConfig.Effort || rec.Metadata.Permissions != op.TargetAgentConfig.Permissions {
		return zero, domain.ErrReplacementConfigChanged
	}
	if m.agentAccounts == nil {
		return zero, ports.ErrAgentAccountUnknown
	}
	selected, err := m.agentAccounts.SessionAccountBinding(ctx, rec.ID, domain.HarnessClaudeCode)
	if err != nil {
		return zero, err
	}
	if selected.AccountID != op.TargetAccountID {
		return zero, domain.ErrReplacementConfigChanged
	}
	ctx = ports.WithAgentAccount(ctx, selected)
	ws := workspaceInfo(rec)
	info, err := os.Stat(ws.Path)
	if err != nil {
		return zero, err
	}
	if !info.IsDir() {
		return zero, domain.ErrReplacementStateInvalid
	}
	// Reuse ordinary native restore, with required replay and a durably reserved
	// generation. That mode forbids both explicit and unanswered fresh fallback.
	ctx = context.WithValue(ctx, replacementNativeResumeKey{}, true)
	launcherEntered = true
	resumed, err := m.resumeChatController(ctx, "orchestrator replacement native probe", rec, project, ws, true, false, p.ResumeGeneration, domain.SessionInterfaceTransitionHistoryStrict)
	if err != nil {
		return zero, err
	}
	current, found, err := m.store.GetSession(ctx, rec.ID)
	if err != nil {
		return zero, err
	}
	if !found || resumed.Mode != RestoreModeNative || current.IsTerminated || current.Metadata.ProviderConversationID != p.BeforeNativeID || current.Metadata.ControllerGeneration != p.ResumeGeneration || current.Metadata.BrowserCapabilityVerifier == "" || current.Metadata.BrowserCapabilityVerifier == p.BeforeCapabilityVerifier {
		return zero, ports.ErrChatRecoveryInconclusive
	}
	latest, found, err := reader.GetActiveOrchestratorReplacement(ctx, op.ProjectID)
	if err != nil {
		return zero, err
	}
	if !found || latest.ID != op.ID || latest.Revision != op.Revision || latest.Phase != op.Phase {
		return zero, domain.ErrReplacementStateInvalid
	}
	project, err = m.loadProject(ctx, op.ProjectID)
	if err != nil {
		return zero, err
	}
	fingerprint, err = domain.ReplacementProjectFingerprint(project)
	if err != nil {
		return zero, err
	}
	account, err := m.agentAccounts.SessionAccountID(ctx, rec.ID, domain.HarnessClaudeCode)
	if err != nil {
		return zero, err
	}
	if !project.ArchivedAt.IsZero() || fingerprint != op.ProjectFingerprint || account != op.TargetAccountID {
		return zero, domain.ErrReplacementConfigChanged
	}
	owner, verifier := current.ControllerOwner(), current.Metadata.BrowserCapabilityVerifier
	if current.HibernatedAt != nil {
		store, ok := m.store.(interface {
			SetSessionHibernated(context.Context, domain.SessionID, int64, *time.Time) (bool, error)
		})
		if !ok {
			return zero, domain.ErrReplacementStateInvalid
		}
		applied, err := store.SetSessionHibernated(ctx, current.ID, current.Revision, nil)
		if err != nil {
			return zero, err
		}
		if !applied {
			return zero, ports.ErrChatRecoveryInconclusive
		}
	}
	current, found, err = m.store.GetSession(ctx, rec.ID)
	if err != nil {
		return zero, err
	}
	if !found || current.HibernatedAt != nil || current.ControllerOwner() != owner || current.Metadata.BrowserCapabilityVerifier != verifier {
		return zero, ports.ErrChatRecoveryInconclusive
	}
	return ports.ReplacementResumeObservation{Owner: current.ControllerOwner(), NativeID: current.Metadata.ProviderConversationID, CapabilityVerifier: current.Metadata.BrowserCapabilityVerifier, HistoryVerified: true, ObservedAt: m.clock().UTC()}, nil
}

// Ordinary orchestrator resumes cannot cross an active replacement. The
// controlled target additionally rechecks its durable intent just before env
// preparation; worker resumes bypass this project-only coordination boundary.
func (m *Manager) guardReplacementNativeResumeLaunch(ctx context.Context, rec domain.SessionRecord, generation string) error {
	if rec.Kind != domain.KindOrchestrator {
		return nil
	}
	reader, ok := m.store.(replacementAdmissionReader)
	if !ok {
		return nil
	}
	op, found, err := reader.GetActiveOrchestratorReplacement(ctx, rec.ProjectID)
	if err != nil || !found {
		return err
	}
	if op.ActivatedSuccessorMatches(rec) {
		checker, ok := m.store.(interface {
			ReplacementActivatedOwner(context.Context, string, domain.SessionRecord) (bool, error)
		})
		if !ok {
			return domain.ErrReplacementIntakeFenced
		}
		matches, err := checker.ReplacementActivatedOwner(ctx, op.ID, rec)
		if err != nil {
			return err
		}
		if !matches {
			return domain.ErrReplacementIntakeFenced
		}
		return nil
	}
	p := op.ResumeEvidence
	if op.Phase != domain.ReplacementVerifyingResume || op.SourceRetiredAt == nil || op.SuccessorSessionID != rec.ID || p == nil || !p.StopConfirmed || p.StopReceipt == nil || p.ResumeIntentAt == nil || p.ResumedAt != nil || generation == "" || generation != p.ResumeGeneration || rec.ControllerOwner() != p.BeforeOwner {
		return domain.ErrReplacementIntakeFenced
	}
	current, found, err := m.store.GetSession(ctx, rec.ID)
	if err != nil {
		return err
	}
	if !found || current.ControllerOwner() != p.BeforeOwner || current.Metadata.BrowserCapabilityVerifier != p.BeforeCapabilityVerifier || !p.StopReceipt.MatchesSource(current) {
		return domain.ErrReplacementSourceChanged
	}
	project, err := m.loadProject(ctx, op.ProjectID)
	if err != nil {
		return err
	}
	fingerprint, err := domain.ReplacementProjectFingerprint(project)
	if err != nil {
		return err
	}
	if !project.ArchivedAt.IsZero() || fingerprint != op.ProjectFingerprint {
		return domain.ErrReplacementConfigChanged
	}
	if m.agentAccounts == nil {
		return ports.ErrAgentAccountUnknown
	}
	account, err := m.agentAccounts.SessionAccountID(ctx, rec.ID, rec.Harness)
	if err != nil {
		return err
	}
	if account != op.TargetAccountID {
		return domain.ErrReplacementConfigChanged
	}
	return ctx.Err()
}
