package store

import (
	"context"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// RecordAccountTransitionRuntime is an exact-generation CAS on the existing
// handoff journal. It cannot attach ownership to a completed/unrelated saga.
func (s *Store) RecordAccountTransitionRuntime(ctx context.Context, id string, state domain.AccountTransitionRuntimeState, launchID, handleID string, at time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	row, err := s.qw.GetSessionInterfaceTransition(ctx, id)
	if err != nil {
		return false, err
	}
	if row.TargetAccountID == "" || row.Phase != domain.SessionInterfaceTransitionTargetStarting {
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
		valid = (previous == domain.AccountTransitionRuntimeReserved || previous == domain.AccountTransitionRuntimeStarted) && row.TargetRuntimeLaunchID == launchID && launchID != "" && (row.TargetRuntimeHandleID == "" || row.TargetRuntimeHandleID == handleID)
	}
	if !valid {
		return false, nil
	}
	n, err := s.qw.RecordAccountTransitionRuntime(ctx, gen.RecordAccountTransitionRuntimeParams{ID: id, State: string(state), LaunchID: launchID, HandleID: handleID, ExpectedState: string(previous), ExpectedLaunchID: row.TargetRuntimeLaunchID, UpdatedAt: at})
	return n > 0, err
}
