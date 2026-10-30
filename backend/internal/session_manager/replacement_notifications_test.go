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

type noticeDeliveryStore struct {
	*fakeStore
	op               domain.OrchestratorReplacement
	receipt          domain.ReplacementNoticeReceipt
	begins, finishes int
	attempted        bool
	onValidate       func()
	onBegin          func()
}

func (s *noticeDeliveryStore) GetOrchestratorReplacement(context.Context, string) (domain.OrchestratorReplacement, bool, error) {
	return s.op, true, nil
}
func (s *noticeDeliveryStore) BeginReplacementWorkerNotice(_ context.Context, id string, revision int64, target domain.SessionID, owner domain.SessionControllerOwner, at time.Time) (domain.ReplacementNoticeReceipt, bool, error) {
	if s.receipt.State == domain.ReplacementNoticeUncertain {
		return s.receipt, false, nil
	}
	s.begins++
	s.receipt = domain.ReplacementNoticeReceipt{OperationID: id, SessionID: target, ContactID: "public-contact", Mode: owner.Mode, Owner: owner, State: domain.ReplacementNoticeUncertain, AttemptID: "private-attempt", Revision: 1}
	if s.onBegin != nil {
		s.onBegin()
		s.onBegin = nil
	}
	return s.receipt, true, nil
}
func (s *noticeDeliveryStore) FinishReplacementWorkerNotice(_ context.Context, _ string, _ domain.SessionID, _ string, _ int64, attempted bool, _ time.Time) (bool, error) {
	s.finishes++
	s.attempted = attempted
	s.receipt.State = domain.ReplacementNoticePending
	if attempted {
		s.receipt.State = domain.ReplacementNoticeUncertain
	}
	return true, nil
}
func (s *noticeDeliveryStore) ValidateReplacementWorkerNotice(ctx context.Context, _ string, target domain.SessionID, _ string, _ int64) (domain.OrchestratorReplacement, domain.SessionRecord, domain.ReplacementNoticeReceipt, error) {
	if s.onValidate != nil {
		s.onValidate()
		s.onValidate = nil
	}
	rec, _, err := s.GetSession(ctx, target)
	if rec.ControllerOwner() != s.receipt.Owner {
		err = domain.ErrReplacementSourceChanged
	}
	return s.op, rec, s.receipt, err
}
func TestReplacementWorkerTUINoticeUsesOneSafeWriteAndNoNudge(t *testing.T) {
	for _, variant := range []string{"idle", "active", "blocked", "draft", "exited", "terminated", "owner rotation", "write error", "stale operation", "missing input fence", "missing inspector"} {
		t.Run(variant, func(t *testing.T) {
			frame := idleTerminalOutput
			if variant == "draft" {
				frame = draftTerminalOutput
			}
			m, msg := composerSendTestManager(t, frame)
			m.SetTerminalInputGate(&fencingInputGate{})
			if variant == "missing input fence" {
				m.SetTerminalInputGate(nil)
			}
			if variant == "missing inspector" {
				m.agents = singleAgent{transitionAgent{}}
			}
			base := m.store.(*fakeStore)
			worker := base.sessions["s1"]
			worker.ProjectID, worker.Kind, worker.ProvisionState = "mer", domain.KindWorker, domain.SessionProvisionReady
			worker.Metadata.RuntimeLaunchID, worker.Metadata.AgentSessionID, worker.Metadata.AgentSessionIDLaunchID = "launch", "native", "launch"
			switch variant {
			case "active":
				worker.Activity.State = domain.ActivityActive
			case "blocked":
				worker.Activity.State = domain.ActivityBlocked
			case "exited":
				worker.Activity.State = domain.ActivityExited
			case "terminated":
				worker.IsTerminated = true
			}
			base.sessions[worker.ID] = worker
			rt := m.runtime.(*transitionRuntime)
			rt.aliveByHandle = map[string]bool{"h1": true}
			store := &noticeDeliveryStore{fakeStore: base, op: domain.OrchestratorReplacement{ID: "operation", ProjectID: "mer", Phase: domain.ReplacementNotifying, Revision: 1}}
			m.store = store
			if variant == "owner rotation" {
				store.onValidate = func() {
					rec := base.sessions[worker.ID]
					rec.Metadata.RuntimeLaunchID = "other-launch"
					base.sessions[worker.ID] = rec
				}
			}
			if variant == "write error" {
				msg.err = errors.New("write reply lost")
			}
			expected := store.op
			if variant == "stale operation" {
				expected.Revision--
			}
			_ = m.DeliverReplacementWorkerFYI(context.Background(), expected, worker.ID)
			writes := variant == "idle" || variant == "write error"
			if writes {
				if len(msg.msgs) != 1 || !store.attempted || store.receipt.State != domain.ReplacementNoticeUncertain {
					t.Fatal("missing uncertain write receipt", len(msg.msgs), store.receipt)
				}
				if id, ok := domain.CoordinationDeliveryID(msg.msgs[0]); !ok || id != store.receipt.NativeDeliveryID() {
					t.Fatal("missing private coordination envelope", msg.msgs)
				}
				_ = m.DeliverReplacementWorkerFYI(context.Background(), expected, worker.ID)
				if len(msg.msgs) != 1 {
					t.Fatal("uncertain TUI was pasted again", msg.msgs)
				}
			} else if len(msg.msgs) != 0 || store.attempted {
				t.Fatal("unsafe worker received FYI", msg.msgs)
			}
			if variant != "idle" && variant != "write error" && variant != "owner rotation" && store.begins != 0 {
				t.Fatal("ordinary no-write consumed a durable attempt", store.begins)
			}
			if rt.destroyed != 0 || len(rt.interrupts) != 0 || rt.created != 0 {
				t.Fatal("FYI changed worker lifecycle", rt)
			}
		})
	}
}

func TestReplacementWorkerFYIRefusesUnverifiedNativeSurface(t *testing.T) {
	for _, stage := range []string{"before reservation", "after reservation", "after validation"} {
		for _, surface := range []string{"native active", "approval", "unknown", "capture failure"} {
			t.Run(stage+"/"+surface, func(t *testing.T) {
				m, msg := composerSendTestManager(t, idleTerminalOutput)
				base := m.store.(*fakeStore)
				worker := base.sessions["s1"]
				worker.ProjectID, worker.Kind, worker.ProvisionState = "mer", domain.KindWorker, domain.SessionProvisionReady
				worker.Metadata.RuntimeLaunchID, worker.Metadata.AgentSessionID, worker.Metadata.AgentSessionIDLaunchID = "launch", "native", "launch"
				base.sessions[worker.ID] = worker
				rt := m.runtime.(*transitionRuntime)
				rt.aliveByHandle = map[string]bool{"h1": true}
				gate := &fencingInputGate{}
				m.SetTerminalInputGate(gate)
				store := &noticeDeliveryStore{fakeStore: base, op: domain.OrchestratorReplacement{ID: "operation", ProjectID: "mer", Phase: domain.ReplacementNotifying, Revision: 1}}
				m.store = store
				unsafe := func() {
					frame := ambiguousTerminalOutput
					switch surface {
					case "native active":
						frame = activeTerminalOutput
					case "approval":
						frame = decisionTerminalOutput
					case "capture failure":
						rt.styledOutputErr = errors.New("native capture failed")
					}
					rt.outputForCall = func(int) string { return frame }
				}
				if stage == "before reservation" {
					unsafe()
				} else if stage == "after validation" {
					store.onValidate = unsafe
				} else {
					store.onBegin = unsafe
				}
				_ = m.DeliverReplacementWorkerFYI(context.Background(), store.op, worker.ID)
				if len(msg.msgs) != 0 || store.attempted {
					t.Fatalf("unverified native surface received FYI: writes=%d attempted=%v state=%s", len(msg.msgs), store.attempted, store.receipt.State)
				}
				if stage == "before reservation" && store.begins != 0 {
					t.Fatal("failed native preflight consumed a notice attempt", store.begins)
				}
				if gate.held != gate.released || base.sessions[worker.ID].ControllerOwner() != worker.ControllerOwner() || base.sessions[worker.ID].Activity.State != domain.ActivityIdle {
					t.Fatal("refused FYI changed worker or leaked its input fence", gate)
				}
			})
		}
	}
}

type contactRotatingWorkspace struct {
	*fakeWorkspace
	rotate func()
}

func (w contactRotatingWorkspace) Create(ctx context.Context, cfg ports.WorkspaceConfig) (ports.WorkspaceInfo, error) {
	result, err := w.fakeWorkspace.Create(ctx, cfg)
	if err == nil {
		w.rotate()
	}
	return result, err
}
func TestReplacementNewWorkerRefreshesContactAfterWorkspacePreparation(t *testing.T) {
	for _, variant := range []string{"tui", "chat", "async chat"} {
		t.Run(variant, func(t *testing.T) {
			chat := &recordingLauncher{}
			m, st, _ := newChatManager(chat)
			m.dataDir = t.TempDir()
			agent := &recordingAgent{}
			m.agents = singleAgent{agent: agent}
			old := domain.SessionRecord{ID: "old-orchestrator", ProjectID: "mer", Kind: domain.KindOrchestrator}
			st.sessions[old.ID] = old
			m.workspace = contactRotatingWorkspace{fakeWorkspace: &fakeWorkspace{}, rotate: func() {
				old.IsTerminated = true
				st.sessions[old.ID] = old
				st.sessions["new-orchestrator"] = domain.SessionRecord{ID: "new-orchestrator", ProjectID: "mer", Kind: domain.KindOrchestrator}
			}}
			mode := domain.SessionModeChat
			if variant == "tui" {
				mode = domain.SessionModeTUI
			}
			var deferred *[]func()
			if variant == "async chat" {
				deferred = deferredBackground(m)
			}
			_, _, _, err := m.Spawn(context.Background(), ports.SpawnConfig{ProjectID: "mer", Kind: domain.KindWorker, Harness: domain.HarnessClaudeCode, RequestedMode: mode, Async: variant == "async chat", Prompt: "keep original task"})
			if err != nil {
				t.Fatal(err)
			}
			if deferred != nil {
				for _, run := range *deferred {
					run()
				}
			}
			prompt := agent.lastLaunch.SystemPrompt
			if mode == domain.SessionModeChat {
				if len(chat.started) != 1 {
					t.Fatal("missing controller", chat.started)
				}
				prompt = chat.started[0].SystemPrompt
			}
			if strings.Contains(prompt, `ao send --session old-orchestrator`) || !strings.Contains(prompt, `ao send --session new-orchestrator`) {
				t.Fatal("launch used stale orchestrator contact")
			}
		})
	}
}
