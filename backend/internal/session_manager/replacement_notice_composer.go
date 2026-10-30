package sessionmanager

import (
	"context"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// Unlike an ordinary operator Send, an automatic FYI needs positive native
// idle/empty proof. Unknown work, provider approvals and capture errors refuse.
func (m *Manager) noticeComposerClear(ctx context.Context, rec domain.SessionRecord, inputAt time.Time) bool {
	gate, _ := m.terminalInputGateFor(rec)
	if _, ok := gate.(TerminalInputFencer); !ok || m.agents == nil {
		return false
	}
	agent, found := m.agents.Agent(rec.Harness)
	if !found {
		return false
	}
	inspector, supported := agent.(ports.TerminalSurfaceInspector)
	output, readable := m.runtime.(ports.StyledTerminalOutputReader)
	if !supported || !readable {
		return false
	}
	poll, settle, limit := m.composerProbeTiming()
	probe, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	if !settleAfterInput(probe, inputAt, settle) {
		return false
	}
	for i := range sendComposerDraftSamples {
		if i > 0 {
			select {
			case <-probe.Done():
				return false
			case <-time.After(poll):
			}
		}
		frame, err := output.GetStyledOutput(probe, runtimeHandle(rec.Metadata), sendComposerProbeLines)
		if err != nil {
			return false
		}
		observed := inspector.InspectTerminalSurface(frame)
		if observed.Work != ports.TerminalSurfaceWorkIdle || observed.Composer != ports.TerminalComposerEmpty {
			return false
		}
	}
	return probe.Err() == nil
}

func (m *Manager) noticeComposerBusyCheck() func(context.Context, domain.SessionRecord) (bool, func()) {
	return func(ctx context.Context, rec domain.SessionRecord) (bool, func()) {
		inputAt, release := m.fenceTerminalInput(rec)
		if release == nil {
			return true, nil
		}
		if !m.noticeComposerClear(ctx, rec, inputAt) {
			release()
			return true, nil
		}
		return false, release
	}
}
