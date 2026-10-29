package agentaccounts

import (
	"context"
	"runtime"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/google/uuid"
)

// currentReviewerExecution rejects a stale trigger after an explicit rebuild.
// Ordinary archived reviewers may resume while they are still the latest owner.
func (s *Service) currentReviewerExecution(ctx context.Context, id string) error {
	store, ok := s.store.(ports.ReviewerExecutionStore)
	if !ok {
		return nil
	} // read-only catalogs cannot create a replacement owner
	old, found, err := store.GetReviewByID(ctx, id)
	if err != nil {
		return ports.ErrAgentAccountStorageUnavailable
	}
	if !found {
		return ports.ErrAgentAccountUnknown
	}
	current, found, err := store.GetReviewBySessionAndHarness(ctx, old.SessionID, old.Harness)
	if err != nil {
		return ports.ErrAgentAccountStorageUnavailable
	}
	if !found || current.ID != old.ID {
		return ports.ErrAgentAccountChanged
	}
	return nil
}

// RebuildReviewer creates a fresh native execution only after explicit archive.
// The old review, conversation, passes and account binding remain immutable.
func (s *Service) RebuildReviewer(ctx context.Context, id, targetID string) (domain.Review, error) {
	if runtime.GOOS != "linux" {
		return domain.Review{}, ports.ErrAgentAccountStorageUnsupported
	}
	if ports.IsRemoteAccountRequest(ctx) {
		return domain.Review{}, ports.ErrAgentAccountLocalOnly
	}
	if targetID == "" {
		return domain.Review{}, ports.ErrAgentAccountUnknown
	}
	store, ok := s.store.(ports.ReviewerExecutionStore)
	if !ok {
		return domain.Review{}, ports.ErrAgentAccountStorageUnavailable
	}
	old, found, err := store.GetReviewByID(ctx, id)
	if err != nil {
		return domain.Review{}, ports.ErrAgentAccountStorageUnavailable
	}
	if !found {
		return domain.Review{}, ports.ErrAgentAccountUnknown
	}
	if err := s.currentReviewerExecution(ctx, id); err != nil {
		return domain.Review{}, err
	}
	if !old.IsArchived || old.ReviewerHandleID != "" || old.ReviewerLaunchID != "" || old.ControllerGeneration != "" {
		return domain.Review{}, &ports.AgentAccountInUseError{Owners: []domain.AgentAccountUsageOwner{{Kind: "reviewer", SessionID: old.SessionID, ReviewID: old.ID, PRURL: old.PRURL, Harness: domain.AgentHarness(old.Harness), Blocking: true}}}
	}
	release, err := s.AcquireAccountUses(ctx, targetID)
	if err != nil {
		return domain.Review{}, err
	}
	defer release()
	selected, err := s.ResolveAccount(ctx, ports.AgentAccountResolutionRequest{Harness: domain.AgentHarness(old.Harness), Role: domain.AgentAccountRoleReviewer, AccountID: targetID})
	if err != nil {
		return domain.Review{}, err
	}
	if selected.Managed() {
		account, err := s.find(ctx, selected.AccountID)
		if err != nil {
			return domain.Review{}, err
		}
		s.read(ctx, account)
		if _, err = s.pick(ctx, domain.AgentHarness(old.Harness), selected.AccountID); err != nil {
			return domain.Review{}, err
		}
	}
	current, created, err := store.CreateReviewerAccountExecution(ctx, old.ID, uuid.NewString(), selected, s.now())
	if err != nil {
		return domain.Review{}, ports.ErrAgentAccountStorageUnavailable
	}
	if !created {
		return domain.Review{}, ports.ErrAgentAccountChanged
	}
	return current, nil
}
