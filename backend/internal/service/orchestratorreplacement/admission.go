package orchestratorreplacement

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
	"github.com/aoagents/agent-orchestrator/backend/internal/service/transcriptexport"
)

const operationDeadline = 30 * time.Minute

func operationNotFound() error {
	return apierr.NotFound("ORCHESTRATOR_REPLACEMENT_NOT_FOUND", "Replacement operation not found")
}

func requestHash(r Request) string {
	b, _ := json.Marshal(struct {
		Version int
		Project domain.ProjectID
		Source  domain.SessionID
	}{1, r.ProjectID, r.SourceSessionID})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func (s *Service) accept(ctx context.Context, request Request) (domain.OrchestratorReplacement, error) {
	var zero domain.OrchestratorReplacement
	key, err := uuid.Parse(request.OperationKey)
	if err != nil || key == uuid.Nil || key.String() != request.OperationKey || strings.TrimSpace(string(request.ProjectID)) == "" || strings.TrimSpace(string(request.SourceSessionID)) == "" {
		return zero, apierr.Invalid("ORCHESTRATOR_REPLACEMENT_REQUEST_INVALID", "Project, source session and a canonical nonzero operation key are required", nil)
	}
	hash := requestHash(request)
	if existing, found, err := s.deps.Store.GetOrchestratorReplacementByKey(ctx, request.OperationKey); err != nil {
		return zero, err
	} else if found {
		if existing.ProjectID != request.ProjectID || existing.SourceSessionID != request.SourceSessionID || existing.RequestHash != hash {
			return existing, operationError(domain.ErrReplacementKeyConflict, existing.ID)
		}
		return existing, rejectionError(existing)
	}
	if active, found, err := s.deps.Store.GetActiveOrchestratorReplacement(ctx, request.ProjectID); err != nil {
		return zero, err
	} else if found {
		return active, operationError(domain.ErrReplacementInProgress, active.ID)
	}
	project, found, err := s.deps.Store.GetProject(ctx, string(request.ProjectID))
	if err != nil {
		return zero, err
	}
	if !found || !project.ArchivedAt.IsZero() {
		return zero, apierr.NotFound("PROJECT_NOT_FOUND", "Project not found")
	}
	source, found, err := s.deps.Store.GetSession(ctx, request.SourceSessionID)
	if err != nil {
		return zero, err
	}
	if !found || source.ProjectID != request.ProjectID {
		return zero, apierr.NotFound("SESSION_NOT_FOUND", "Source session not found in project")
	}
	if source.Kind != domain.KindOrchestrator || source.IsTerminated {
		return zero, apierr.Conflict("ORCHESTRATOR_REPLACEMENT_SOURCE_INVALID", "Source must be the live project orchestrator", nil)
	}
	now := s.deps.Now().UTC()
	op := domain.OrchestratorReplacement{SchemaVersion: 1, ID: uuid.NewString(), OperationKey: request.OperationKey, RequestHash: hash, ProjectID: request.ProjectID, SourceSessionID: source.ID, SourceOwner: source.ControllerOwner(), SourceCapabilityVerifier: source.Metadata.BrowserCapabilityVerifier, Phase: domain.ReplacementAccepted, CreatedAt: now, UpdatedAt: now, DeadlineAt: now.Add(operationDeadline)}
	op.ProjectFingerprint, err = domain.ReplacementProjectFingerprint(project)
	if err != nil {
		return zero, err
	}
	if source.Harness != domain.HarnessClaudeCode {
		return s.reject(ctx, op, "ORCHESTRATOR_HANDOFF_SOURCE_UNSUPPORTED", "Phase 1 requires a Claude Code source orchestrator")
	}
	if project.Config.Orchestrator.Harness != domain.HarnessClaudeCode {
		return s.reject(ctx, op, "ORCHESTRATOR_HANDOFF_TARGET_UNSUPPORTED", "Project orchestrator configuration must select Claude Code Chat")
	}
	snapshot, err := s.deps.Store.LoadTranscriptSourceSnapshot(ctx, source.ID)
	if err != nil {
		return zero, err
	}
	_, historyErr := transcriptexport.RequiredNativeEpochs(snapshot)
	var nativeErr *ports.NativeTranscriptError
	if errors.As(historyErr, &nativeErr) && nativeErr.Code == "ORCHESTRATOR_HANDOFF_HISTORY_UNSUPPORTED" {
		return s.reject(ctx, op, nativeErr.Code, nativeErr.Reason)
	}
	if s.deps.Runtime == nil || s.deps.Documents == nil {
		return zero, apierr.Unavailable("ORCHESTRATOR_REPLACEMENT_UNAVAILABLE", "Replacement dependencies are unavailable")
	}
	target, err := s.deps.Runtime.PreflightReplacementTarget(ctx, project)
	if err != nil || target.AccountID == "" {
		return s.reject(ctx, op, "ORCHESTRATOR_HANDOFF_TARGET_UNAVAILABLE", "Claude Code Chat and native resume must be available before handoff")
	}
	op.TargetAgentConfig, op.TargetAccountID = target.AgentConfig, target.AccountID
	info, err := s.deps.Documents.Info(ctx, request.ProjectID)
	if err != nil {
		return zero, err
	}
	op.SharedDir, op.DocumentPath = info.SharedDir, info.DocumentPath
	if historyErr != nil {
		op.Error = &domain.ReplacementFailure{Code: "ORCHESTRATOR_HANDOFF_HISTORY_UNVERIFIED", Message: "Required native history is not yet verifiable", Retryable: true}
	}
	result, _, err := s.deps.Store.CreateOrchestratorReplacement(ctx, op)
	if err != nil {
		return result, operationError(err, result.ID)
	}
	return result, rejectionError(result)
}

func (s *Service) reject(ctx context.Context, op domain.OrchestratorReplacement, code, message string) (domain.OrchestratorReplacement, error) {
	op.Phase = domain.ReplacementRejected
	op.Error = &domain.ReplacementFailure{Code: code, Message: message}
	result, _, err := s.deps.Store.CreateOrchestratorReplacement(ctx, op)
	if err != nil {
		return result, operationError(err, result.ID)
	}
	return result, rejectionError(result)
}

func rejectionError(op domain.OrchestratorReplacement) error {
	if op.Phase != domain.ReplacementRejected {
		return nil
	}
	if op.Error == nil {
		return operationError(domain.ErrReplacementStateInvalid, op.ID)
	}
	return apierr.Conflict(op.Error.Code, op.Error.Message, map[string]any{"operationId": op.ID})
}

func operationError(err error, id string) error {
	details := map[string]any{"operationId": id}
	switch {
	case errors.Is(err, domain.ErrReplacementKeyConflict):
		return apierr.Conflict("ORCHESTRATOR_REPLACEMENT_KEY_CONFLICT", "Operation key belongs to different intent", details)
	case errors.Is(err, domain.ErrReplacementInProgress):
		return apierr.Conflict("ORCHESTRATOR_REPLACEMENT_IN_PROGRESS", "Project already has an active replacement", details)
	case errors.Is(err, domain.ErrReplacementSourceChanged):
		return apierr.Conflict("ORCHESTRATOR_REPLACEMENT_SOURCE_CHANGED", "Source ownership changed before admission", details)
	case errors.Is(err, domain.ErrReplacementConfigChanged):
		return apierr.Conflict("ORCHESTRATOR_REPLACEMENT_CONFIG_CHANGED", "Project settings changed during preflight", details)
	default:
		return err
	}
}
