package projectdocuments

import (
	"context"
	"crypto/subtle"
	"log/slog"
	"strconv"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
)

func (s *Service) authorize(ctx context.Context, id domain.ProjectID, a Actor) (domain.SessionRecord, error) {
	rec, ok, err := s.store.GetSession(ctx, a.SessionID)
	if err != nil {
		return rec, err
	}
	if !ok || rec.ProjectID != id || rec.IsTerminated || rec.Kind != domain.KindOrchestrator || s.capabilities == nil || !s.capabilities.Valid(rec.ID, a.Capability, rec.Metadata.BrowserCapabilityVerifier) {
		return rec, apierr.Forbidden("PROJECT_DOCUMENT_WRITER_REQUIRED", "Only the current orchestrator capability may write project documents")
	}
	sessions, err := s.store.ListSessions(ctx, id)
	if err != nil {
		return rec, err
	}
	for _, other := range sessions {
		if other.Kind == domain.KindOrchestrator && !other.IsTerminated && other.ID != rec.ID {
			return rec, conflict("PROJECT_DOCUMENT_OWNER_AMBIGUOUS", "More than one orchestrator is active")
		}
	}
	if rec.Mode == domain.SessionModeChat && rec.Metadata.ControllerGeneration == "" || rec.Mode != domain.SessionModeChat && rec.Metadata.RuntimeLaunchID == "" {
		return rec, conflict("PROJECT_DOCUMENT_CONTROLLER_UNAVAILABLE", "The current controller identity is not ready")
	}
	return rec, nil
}
func (s *Service) validate(ctx context.Context, id domain.ProjectID, a Actor, g Grant, state domain.ProjectDocumentState, allowFrozen bool) (domain.SessionRecord, error) {
	rec, err := s.authorize(ctx, id, a)
	if err != nil {
		return rec, err
	}
	lease := state.Lease
	if lease == nil || lease.SessionID != rec.ID || lease.Owner != rec.ControllerOwner() || lease.Epoch != g.Epoch || subtle.ConstantTimeCompare([]byte(lease.TokenHash), []byte(hash([]byte(g.Token)))) != 1 || g.Token == "" {
		return rec, apierr.Forbidden("PROJECT_DOCUMENT_LEASE_INVALID", "The writer lease is stale or invalid")
	}
	if lease.Frozen && !allowFrozen {
		return rec, conflict("PROJECT_DOCUMENT_LEASE_FROZEN", "Project documents are frozen for handoff")
	}
	return rec, nil
}

func grantFor(id domain.ProjectID, a Actor, epoch int64) Grant {
	return Grant{Epoch: epoch, Token: hash([]byte("AO-project-documents\x00" + string(id) + "\x00" + string(a.SessionID) + "\x00" + strconv.FormatInt(epoch, 10) + "\x00" + a.Capability))}
}

// Acquire returns the current controller lease, reclaiming an unfrozen lease
// only when its previous session is conclusively terminated.
func (s *Service) Acquire(ctx context.Context, id domain.ProjectID, a Actor) (Grant, error) {
	unlock := s.lock(id)
	defer unlock()
	rec, err := s.authorize(ctx, id, a)
	if err != nil {
		return Grant{}, err
	}
	state, root, _, err := s.state(ctx, id)
	if err != nil {
		return Grant{}, err
	}
	epoch := int64(1)
	if lease := state.Lease; lease != nil {
		if lease.Frozen {
			return Grant{}, conflict("PROJECT_DOCUMENT_LEASE_FROZEN", "Project documents are frozen")
		}
		if lease.SessionID != rec.ID && (lease.TokenHash != "" || lease.ClaimVerifier != "" || lease.ReplacementID != "") {
			previous, ok, err := s.store.GetSession(ctx, lease.SessionID)
			if err != nil {
				return Grant{}, err
			}
			if !ok || !previous.IsTerminated {
				return Grant{}, conflict("PROJECT_DOCUMENT_LEASE_HELD", "Project lease is held by session "+string(lease.SessionID))
			}
		} else if lease.Owner == rec.ControllerOwner() && (lease.TokenHash != "" || lease.ClaimVerifier != "") {
			grant := grantFor(id, a, lease.Epoch)
			if lease.ClaimVerifier != "" {
				if lease.ClaimVerifier != rec.Metadata.BrowserCapabilityVerifier || lease.TokenHash != "" || lease.ReplacementID == "" {
					return Grant{}, apierr.Forbidden("PROJECT_DOCUMENT_LEASE_INVALID", "Pending writer claim is stale")
				}
				published, err := s.store.WithProjectDocumentPublication(ctx, id, rec, state.Revision, func(reserved context.Context) error {
					state.Lease.TokenHash, state.Lease.ClaimVerifier = hash([]byte(grant.Token)), ""
					return s.save(reserved, id, &state)
				})
				if err != nil {
					return Grant{}, err
				}
				if !published {
					return Grant{}, conflict("PROJECT_DOCUMENT_STATE_CHANGED", "Writer claim changed")
				}
				return grant, nil
			}
			if subtle.ConstantTimeCompare([]byte(lease.TokenHash), []byte(hash([]byte(grant.Token)))) != 1 {
				return Grant{}, apierr.Forbidden("PROJECT_DOCUMENT_LEASE_INVALID", "Controller capability no longer matches the lease")
			}
			return grant, nil
		}
		epoch = lease.Epoch + 1
	}
	grant := grantFor(id, a, epoch)
	if err := s.ensureRoot(root); err != nil {
		return Grant{}, err
	}
	previous := state.Lease
	state.Lease = &domain.ProjectDocumentLease{SessionID: rec.ID, Owner: rec.ControllerOwner(), Epoch: epoch, TokenHash: hash([]byte(grant.Token))}
	if err := s.save(ctx, id, &state); err != nil {
		return Grant{}, err
	}
	if previous != nil && previous.SessionID != rec.ID {
		slog.InfoContext(ctx, "Reclaimed project document lease from inactive or revoked writer",
			"project_id", id, "previous_session_id", previous.SessionID, "previous_epoch", previous.Epoch,
			"session_id", rec.ID, "epoch", epoch)
	}
	return grant, nil
}

// Freeze drains managed writes and durably fences the source lease.
func (s *Service) Freeze(ctx context.Context, id domain.ProjectID, a Actor, g Grant) error {
	unlock := s.lock(id)
	defer unlock()
	state, _, _, err := s.state(ctx, id)
	if err != nil {
		return err
	}
	if _, err := s.validate(ctx, id, a, g, state, true); err != nil {
		return err
	}
	if state.Lease.Frozen {
		return nil
	}
	state.Lease.Frozen = true
	return s.save(ctx, id, &state)
}

// Transfer is the internal handoff boundary. Call only after conclusive source
// retirement and successor verification; it never steals an unfrozen lease.
func (s *Service) Transfer(ctx context.Context, id domain.ProjectID, previous Grant, target Actor) (Grant, error) {
	unlock := s.lock(id)
	defer unlock()
	state, _, _, err := s.state(ctx, id)
	if err != nil {
		return Grant{}, err
	}
	old := state.Lease
	if old == nil || !old.Frozen || old.Epoch != previous.Epoch || subtle.ConstantTimeCompare([]byte(old.TokenHash), []byte(hash([]byte(previous.Token)))) != 1 {
		return Grant{}, conflict("PROJECT_DOCUMENT_TRANSFER_STALE", "Frozen predecessor lease is required")
	}
	source, ok, err := s.store.GetSession(ctx, old.SessionID)
	if err != nil {
		return Grant{}, err
	}
	if !ok || !source.IsTerminated {
		return Grant{}, conflict("PROJECT_DOCUMENT_SOURCE_NOT_RETIRED", "The predecessor has not retired")
	}
	rec, err := s.authorize(ctx, id, target)
	if err != nil {
		return Grant{}, err
	}
	grant := grantFor(id, target, old.Epoch+1)
	state.Lease = &domain.ProjectDocumentLease{SessionID: rec.ID, Owner: rec.ControllerOwner(), Epoch: old.Epoch + 1, TokenHash: hash([]byte(grant.Token))}
	if err := s.save(ctx, id, &state); err != nil {
		return Grant{}, err
	}
	return grant, nil
}

// publish holds a durable writer reservation from final authorization through
// filesystem publication. Controller/capability rotation cannot pass this boundary.
func (s *Service) publish(ctx context.Context, id domain.ProjectID, actor Actor, grant Grant, fn func(context.Context) error) error {
	current, _, _, err := s.state(ctx, id)
	if err != nil {
		return err
	}
	rec, err := s.validate(ctx, id, actor, grant, current, false)
	if err != nil {
		return err
	}
	published, err := s.store.WithProjectDocumentPublication(ctx, id, rec, current.Revision, fn)
	if err != nil {
		return err
	}
	if !published {
		return apierr.Forbidden("PROJECT_DOCUMENT_LEASE_INVALID", "The writer changed before publication")
	}
	return nil
}
