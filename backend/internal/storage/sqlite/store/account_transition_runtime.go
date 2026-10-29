package store

import (
	"context"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// RecordAccountTransitionRuntime is an exact-generation CAS on the existing
// handoff journal. Terminal recovery can only retire an existing receipt; it
// cannot reserve a new generation or attach ownership to an unrelated saga.
func (s *Store) RecordAccountTransitionRuntime(ctx context.Context, id string, state domain.AccountTransitionRuntimeState, launchID, handleID string, at time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	row, err := s.qw.GetSessionInterfaceTransition(ctx, id)
	if err != nil {
		return false, err
	}
	if row.TargetAccountID == "" {
		return false, nil
	}
	switch row.Phase {
	case domain.SessionInterfaceTransitionTargetStarting:
	case domain.SessionInterfaceTransitionActivating, domain.SessionInterfaceTransitionRecovery:
		if state != domain.AccountTransitionRuntimeStopped {
			return false, nil
		}
	default:
		return false, nil
	}
	previous := domain.AccountTransitionRuntimeState(row.TargetRuntimeState)
	if previous == state && row.TargetRuntimeLaunchID == launchID && row.TargetRuntimeHandleID == handleID {
		return true, nil
	}
	valid := false
	switch state {
	case domain.AccountTransitionRuntimeReserved:
		valid = (previous == domain.AccountTransitionRuntimeNotStarted || previous == domain.AccountTransitionRuntimeStopped) && launchID != "" && handleID == ""
	case domain.AccountTransitionRuntimeStarted:
		valid = previous == domain.AccountTransitionRuntimeReserved && row.TargetRuntimeLaunchID == launchID && launchID != "" && handleID != ""
	case domain.AccountTransitionRuntimeStopped:
		valid = (previous == domain.AccountTransitionRuntimeReserved || previous == domain.AccountTransitionRuntimeStarted || previous == "") && row.TargetRuntimeLaunchID == launchID && (row.TargetRuntimeHandleID == "" || row.TargetRuntimeHandleID == handleID)
	}
	if !valid {
		return false, nil
	}
	n, err := s.qw.RecordAccountTransitionRuntime(ctx, gen.RecordAccountTransitionRuntimeParams{ID: id, State: string(state), LaunchID: launchID, HandleID: handleID, ExpectedState: string(previous), ExpectedLaunchID: row.TargetRuntimeLaunchID, UpdatedAt: at})
	return n > 0, err
}

// ListUnsettledAccountTransitions finds historical recovery obligations left by
// older daemons as well as terminal write failures. Committed live targets are
// legitimate session owners and must never be stopped by this sweep.
func (s *Store) ListUnsettledAccountTransitions(ctx context.Context) ([]domain.SessionInterfaceTransition, error) {
	rows, err := s.qr.ListUnsettledAccountTransitions(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]domain.SessionInterfaceTransition, 0, len(rows))
	for _, row := range rows {
		out = append(out, interfaceTransitionToDomain(row))
	}
	return out, nil
}

// RetireAccountTransitionRuntime rechecks the shared recovery projection and
// retires a historical receipt atomically, without touching a runtime namespace.
func (s *Store) RetireAccountTransitionRuntime(ctx context.Context, id string, at time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	row, err := s.qw.GetSessionInterfaceTransition(ctx, id)
	if err != nil {
		return false, err
	}
	if row.TargetRuntimeState == string(domain.AccountTransitionRuntimeStopped) {
		return true, nil
	}
	n, err := s.qw.RetireAccountTransitionRuntime(ctx, gen.RetireAccountTransitionRuntimeParams{ID: id, UpdatedAt: at})
	return n > 0, err
}

// HasUnconfirmedAccountTransition reports a durable shutdown failure which
// still needs recovery, rather than an already superseded historical notice.
func (s *Store) HasUnconfirmedAccountTransition(ctx context.Context, id domain.SessionID) (bool, error) {
	return s.qr.HasUnconfirmedAccountTransition(ctx, id)
}
