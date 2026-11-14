package sessionmanager

import (
	"errors"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type reportingObserverFunc func(ports.HCGReportingLaunch)

func (f reportingObserverFunc) ReportingLaunchReady(n ports.HCGReportingLaunch) { f(n) }

func TestReportingLaunchTUIMarksFinalSpawnAndRestore(t *testing.T) {
	m, st, _, ws := newManager()
	ws.path = t.TempDir()
	m.dataDir = t.TempDir()
	var notices []ports.HCGReportingLaunch
	m.hcgReporting = reportingObserverFunc(func(n ports.HCGReportingLaunch) {
		rec := st.sessions[n.SessionID]
		if rec.ControllerOwner() != n.CommittedOwner || rec.IsTerminated || n.CommittedOwner.RuntimeLaunchID == "" {
			t.Errorf("notification before final durable owner: %+v", n)
		}
		notices = append(notices, n)
	})
	rec, _, _, err := m.Spawn(t.Context(), ports.SpawnConfig{ProjectID: "mer", Kind: domain.KindWorker, Harness: domain.HarnessClaudeCode, Prompt: "synthetic task"})
	if err != nil {
		t.Fatal(err)
	}
	if len(notices) != 1 || !notices[0].Fresh || notices[0].Origin != ports.HCGReportingMainTUI {
		t.Fatalf("spawn notifications=%+v", notices)
	}
	stored := st.sessions[rec.ID]
	stored.IsTerminated = true
	st.sessions[rec.ID] = stored
	if _, err := m.RestoreWithMode(t.Context(), rec.ID); err != nil {
		t.Fatal(err)
	}
	if len(notices) != 2 || notices[0].CommittedOwner.RuntimeLaunchID == notices[1].CommittedOwner.RuntimeLaunchID {
		t.Fatalf("restore notifications=%+v", notices)
	}
}
func TestReportingLaunchTUIPromptFailureAfterMarkSpawned(t *testing.T) {
	st := newFakeStore()
	st.projects["mer"] = domain.ProjectRecord{ID: "mer", Config: testRoleAgents()}
	lcm := &fakeLCM{store: st}
	count := 0
	m := New(Deps{Runtime: &fakeRuntime{}, Agents: singleAgent{agent: afterStartAgent{recordingAgent: &recordingAgent{}}}, Workspace: &fakeWorkspace{path: t.TempDir()}, Store: st, Messenger: &fakeMessenger{err: errors.New("delivery failed")}, Lifecycle: lcm, LookPath: func(string) (string, error) { return "/bin/true", nil }, HCGReporting: reportingObserverFunc(func(ports.HCGReportingLaunch) { count++ })})
	if _, _, _, err := m.Spawn(t.Context(), ports.SpawnConfig{ProjectID: "mer", Kind: domain.KindWorker, Harness: domain.HarnessClaudeCode, Prompt: "synthetic task"}); err == nil {
		t.Fatal("expected native prompt failure")
	}
	if lcm.completed != 1 || count != 0 {
		t.Fatalf("MarkSpawned=%d notifications=%d", lcm.completed, count)
	}
}
func TestReportingLaunchTUIPanicDoesNotUndoSuccess(t *testing.T) {
	m, _, _, ws := newManager()
	ws.path = t.TempDir()
	m.hcgReporting = reportingObserverFunc(func(ports.HCGReportingLaunch) { panic("optional reporting failure") })
	if _, _, _, err := m.Spawn(t.Context(), ports.SpawnConfig{ProjectID: "mer", Kind: domain.KindWorker, Harness: domain.HarnessClaudeCode}); err != nil {
		t.Fatal(err)
	}
}
