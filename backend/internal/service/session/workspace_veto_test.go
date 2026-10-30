package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/lifecycle"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	sessionmanager "github.com/aoagents/agent-orchestrator/backend/internal/session_manager"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

type vetoPathRuntime struct {
	ports.Runtime
	alive              map[string]bool
	destroyed, created int
}

func (r *vetoPathRuntime) ProbeFencedRuntime(context.Context, ports.FencedRuntimeRef) ports.FencedProbeResult {
	return ports.FencedProbeResult{Liveness: ports.FencedUnknown}
}
func (r *vetoPathRuntime) Destroy(_ context.Context, h ports.RuntimeHandle) error {
	r.destroyed++
	r.alive[h.ID] = false
	return nil
}
func (r *vetoPathRuntime) Create(context.Context, ports.RuntimeConfig) (ports.RuntimeHandle, error) {
	r.created++
	return ports.RuntimeHandle{}, errors.New("unexpected launch")
}

type vetoPathWorkspace struct {
	ports.Workspace
	marker             string
	destroyed, stashed int
}

func (w *vetoPathWorkspace) Destroy(context.Context, ports.WorkspaceInfo) error {
	w.destroyed++
	return os.Remove(w.marker)
}
func (w *vetoPathWorkspace) ForceDestroy(ctx context.Context, in ports.WorkspaceInfo) error {
	return w.Destroy(ctx, in)
}
func (w *vetoPathWorkspace) StashUncommitted(context.Context, ports.WorkspaceInfo) (string, error) {
	w.stashed++
	return "", nil
}

type vetoPathAgents struct{}

func (vetoPathAgents) Agent(domain.AgentHarness) (ports.Agent, bool) { return nil, false }

func TestSharedAbandonedWriterBlocksLiveSessionWorkspaceTeardown(t *testing.T) {
	for _, action := range []string{"project-teardown", "kill", "request-kill", "replacement", "shutdown-save"} {
		t.Run(action, func(t *testing.T) {
			ctx := t.Context()
			now := time.Now()
			dir := t.TempDir()
			st := sqlitetest.MustOpenAt(t, dir)
			if err := st.UpsertProject(ctx, domain.ProjectRecord{ID: "p", Kind: domain.ProjectKindScratch, Path: dir, RegisteredAt: now}); err != nil {
				t.Fatal(err)
			}
			unknown, err := st.CreateSession(ctx, domain.SessionRecord{ProjectID: "p", Kind: domain.KindWorker, Harness: domain.HarnessCodex, Mode: domain.SessionModeTUI, Metadata: domain.SessionMetadata{WorkspacePath: dir, Model: "native", RuntimeHandleID: "source-runtime", RuntimeLaunchID: "source-generation", AgentSessionID: "native-thread", AgentSessionIDLaunchID: "source-generation"}, CreatedAt: now, UpdatedAt: now})
			if err != nil {
				t.Fatal(err)
			}
			if err = st.BindSessionAgentAccount(ctx, unknown.ID, domain.AgentAccountProviderCodex, "device-codex", now); err != nil {
				t.Fatal(err)
			}
			current, err := st.SessionAgentSettings(ctx, unknown.ID)
			if err != nil {
				t.Fatal(err)
			}
			target := current.Settings
			target.Effort = "high"
			op, err := st.BeginAgentSettingsOperation(ctx, domain.AgentSettingsOperation{ID: "lost-target", SessionID: unknown.ID, IdempotencyKey: "lost-target", Source: current.Settings, Target: target, AccountID: current.AccountID, ControllerGeneration: current.OwnerGeneration, ProviderConversationID: current.ProviderConversationID, Actor: domain.AgentSettingsActor{Kind: domain.AgentSettingsActorLocal}, ApplyMode: domain.AgentSettingsRestart, CreatedAt: now})
			if err != nil {
				t.Fatal(err)
			}
			proof := domain.AgentSettingsExecution{Owner: domain.AgentSettingsOwner{SessionID: unknown.ID, AccountID: current.AccountID, ControllerGeneration: current.OwnerGeneration, ProviderConversationID: current.ProviderConversationID}, Revision: current.Revision, Qualification: domain.AgentSettingsQualification{InputFingerprint: "fake-input", PolicyVersion: "test"}, Plan: domain.AgentSettingsProviderPlan{Protocol: "codex-native-resume", ProviderConversationID: current.ProviderConversationID, Source: current.Settings, Target: target, Before: domain.AgentSettingsNativeValues{Model: "native", Effort: "low", Fast: "default"}, After: domain.AgentSettingsNativeValues{Model: "native", Effort: "high", Fast: "default"}}}
			op, err = st.PinAgentSettingsExecution(ctx, unknown.ID, op.ID, op.Version, proof, now)
			if err != nil {
				t.Fatal(err)
			}
			op, err = st.AdvanceAgentSettingsOperation(ctx, unknown.ID, op.ID, op.Version, domain.AgentSettingsApplyingModel, now)
			if err != nil {
				t.Fatal(err)
			}
			if err = st.SetTUIAgentSettingsRuntimeUnknown(ctx, unknown.ID, op.ID, true); err != nil {
				t.Fatal(err)
			}
			op, err = st.FinishAgentSettingsOperationWithCode(ctx, unknown.ID, op.ID, op.Version, domain.AgentSettingsRecoveryRequired, domain.AgentSettingsErrorRecovery, "AGENT_SETTINGS_DAEMON_RESTARTED", now)
			if err != nil {
				t.Fatal(err)
			}
			lc := lifecycle.New(st, nil)
			if err = lc.MarkTerminated(ctx, unknown.ID); err != nil {
				t.Fatal(err)
			}
			if _, err = st.AbandonTerminatedTUISettings(ctx, unknown.ID, op.ID, op.Version, false, now); err != nil {
				t.Fatal(err)
			}
			sibling, err := st.CreateSession(ctx, domain.SessionRecord{ProjectID: "p", Kind: domain.KindOrchestrator, Harness: domain.HarnessCodex, Mode: domain.SessionModeTUI, Metadata: domain.SessionMetadata{WorkspacePath: dir, Branch: "ao/live", RuntimeHandleID: "live-runtime", RuntimeLaunchID: "live-generation"}, CreatedAt: now, UpdatedAt: now})
			if err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(dir, "retained-work.txt")
			if err = os.WriteFile(marker, []byte("unknown writer data"), 0o600); err != nil {
				t.Fatal(err)
			}
			rt := &vetoPathRuntime{alive: map[string]bool{"unknown-target": true, "live-runtime": true}}
			ws := &vetoPathWorkspace{marker: marker}
			manager := sessionmanager.New(sessionmanager.Deps{Store: st, Lifecycle: lc, Runtime: rt, Workspace: ws, Agents: vetoPathAgents{}, DataDir: dir})
			service := New(manager, st)
			switch action {
			case "project-teardown":
				err = service.TeardownProject(ctx, "p")
			case "kill":
				_, err = manager.Kill(ctx, sibling.ID)
			case "request-kill":
				_, err = manager.RequestKill(ctx, sibling.ID)
			case "replacement":
				err = manager.RetireForReplacement(ctx, sibling.ID)
			case "shutdown-save":
				err = manager.SaveAndTeardownAll(ctx)
			}
			if err != nil {
				t.Fatalf("retained workspace must not prevent stopping the live session: %v", err)
			}
			wantStopped := action != "shutdown-save"

			data, readErr := os.ReadFile(marker)
			if readErr != nil || string(data) != "unknown writer data" || ws.destroyed != 0 || ws.stashed != 0 || (wantStopped && rt.destroyed < 1) || (!wantStopped && rt.destroyed != 0) || !rt.alive["unknown-target"] || rt.created != 0 {
				t.Fatalf("shared veto bypassed: action=%s Destroy=%d Stash=%d runtimeDestroy=%d unknownAlive=%v newCreate=%d file=%q readErr=%v", action, ws.destroyed, ws.stashed, rt.destroyed, rt.alive["unknown-target"], rt.created, data, readErr)
			}
			live, _, err := st.GetSession(ctx, sibling.ID)
			if err != nil || live.IsTerminated != wantStopped || rt.alive["live-runtime"] == wantStopped {
				t.Fatalf("workspace retention must preserve stop intent: terminated=%v runtimeAlive=%v wantStopped=%v err=%v", live.IsTerminated, rt.alive["live-runtime"], wantStopped, err)
			}
		})
	}
}
