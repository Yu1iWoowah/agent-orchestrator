package sessionmanager

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type abandonedWorkspaceStore struct{ *fakeStore }

var _ tuiSettingsTerminationStore = abandonedWorkspaceStore{}

func (abandonedWorkspaceStore) PendingAgentSettingsOperation(context.Context, domain.SessionID) (domain.AgentSettingsOperation, bool, error) {
	return domain.AgentSettingsOperation{}, false, errors.New("unexpected settings operation lookup")
}

func (abandonedWorkspaceStore) AbandonTerminatedTUISettings(context.Context, domain.SessionID, string, int64, bool, time.Time) (domain.AgentSettingsOperation, error) {
	return domain.AgentSettingsOperation{}, errors.New("unexpected settings operation settlement")
}

func (abandonedWorkspaceStore) HasAbandonedTUISettingsWriter(_ context.Context, id domain.SessionID) (bool, error) {
	return id == "unknown", nil
}

func TestWorkspaceRowFailurePreservesSharedAbandonedWriter(t *testing.T) {
	m, st, _, ws := newManager()
	project := domain.ProjectRecord{ID: "mer", Kind: domain.ProjectKindWorkspace, Path: "/repo/mer"}
	st.projects["mer"] = project
	st.upsertWTErr = errors.New("db locked")
	ws.path = "/ws/shared"
	st.sessions["unknown"] = domain.SessionRecord{ID: "unknown", ProjectID: "mer", IsTerminated: true, Metadata: domain.SessionMetadata{WorkspacePath: ws.path}}
	m.store = abandonedWorkspaceStore{st}
	_, retained, err := m.createSessionWorkspace(t.Context(), project, ports.SpawnConfig{ProjectID: "mer", Kind: domain.KindWorker}, "attempt", "ao/attempt", nil)
	if err == nil || !strings.Contains(err.Error(), "record workspace worktree") {
		t.Fatalf("want row persistence error, got %v", err)
	}
	if ws.projectDestroyed != 0 || ws.destroyed != 0 || retained == nil {
		t.Fatalf("row rollback reclaimed abandoned writer path: projectDestroy=%d Destroy=%d retained=%v", ws.projectDestroyed, ws.destroyed, retained != nil)
	}
}
