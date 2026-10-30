package sessionmanager

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/sessionguard"
)

func TestReplacementTerminalInputRefusesSourceButKeepsWorkerAndInternalLease(t *testing.T) {
	f := newReplacementInputTUIFixture(t)
	if release, err := f.manager.AcquireTerminalInput(context.Background(), f.op.SourceSessionID); !errors.Is(err, domain.ErrReplacementIntakeFenced) {
		if release != nil {
			release()
		}
		t.Fatalf("raw source input crossed replacement: %v", err)
	}
	worker := mkLive("mer-worker")
	worker.Kind = domain.KindWorker
	f.store.sessions[worker.ID] = worker
	release, err := f.manager.AcquireTerminalInput(context.Background(), worker.ID)
	if err != nil {
		t.Fatalf("worker terminal fenced: %v", err)
	}
	release()
	internal, ok := f.manager.AcquireSessionInput(f.op.SourceSessionID)
	if !ok {
		t.Fatal("replacement control cannot acquire internal input lease")
	}
	internal()
}

type replacementIntakeChat struct {
	*recordingLauncher
	inputs []ports.ChatUserMessage
}

func (c *replacementIntakeChat) QueueReplacementIntake(_ context.Context, id domain.SessionID, msg ports.ChatUserMessage) (domain.ConversationTurn, error) {
	c.inputs = append(c.inputs, msg)
	return domain.ConversationTurn{ID: "queued-turn", HandledBySessionID: id, State: domain.TurnStateQueued}, nil
}

func TestReplacementTUIOrdinarySendQueuesButReportAndInterruptRemainDeferred(t *testing.T) {
	for _, action := range []string{"ordinary", "retired", "semantic report", "interrupt"} {
		t.Run(action, func(t *testing.T) {
			f := newReplacementInputTUIFixture(t)
			chat := &replacementIntakeChat{recordingLauncher: &recordingLauncher{}}
			f.manager.chat = chat
			if action == "retired" {
				now := time.Now().UTC()
				f.store.operation.SourceRetiredAt = &now
				f.store.operation.Phase = domain.ReplacementStarting
				rec := f.store.sessions[f.op.SourceSessionID]
				rec.IsTerminated = true
				f.store.sessions[rec.ID] = rec
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f.messenger.onSend = func(domain.SessionID, string) { cancel() }
			var err error
			switch action {
			case "ordinary", "retired":
				err = f.manager.SendWithOptions(ctx, f.op.SourceSessionID, "preserve this instruction", nil, ports.MessageDeliveryOptions{AuthoredByUser: true, SenderSessionID: "worker-source"})
			case "semantic report":
				err = f.manager.SendSemantic(ctx, f.op.SourceSessionID, "pending report", "report-batch:original")
			case "interrupt":
				err = f.manager.InterruptTUI(ctx, f.op.SourceSessionID)
			}
			if action == "ordinary" || action == "retired" {
				if err != nil || len(chat.inputs) != 1 || chat.inputs[0].Text != "preserve this instruction" || !chat.inputs[0].AuthoredByUser || chat.inputs[0].SenderSessionID != "worker-source" || chat.inputs[0].ClientMessageID == "" {
					t.Fatalf("ordinary input not queued with identity: %+v %v", chat.inputs, err)
				}
			} else if !errors.Is(err, domain.ErrReplacementIntakeFenced) || len(chat.inputs) != 0 {
				t.Fatalf("%s not deferred: queued=%v err=%v", action, chat.inputs, err)
			}
			if len(f.messenger.msgs) != 0 || len(f.runtime.interrupts) != 0 {
				t.Fatal("replacement input reached source pane")
			}
		})
	}
}

func TestReplacementAdmissionWaitsUntilAdmittedTerminalWriteReleases(t *testing.T) {
	f := newReplacementInputTUIFixture(t)
	f.store.operation.Phase = domain.ReplacementCompleted
	release, err := f.manager.AcquireTerminalInput(context.Background(), f.op.SourceSessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	entered := false
	err = f.manager.WithOrchestratorProjectGate(ctx, f.op.ProjectID, func(context.Context) error { entered = true; return nil })
	if !errors.Is(err, context.DeadlineExceeded) || entered {
		t.Fatalf("admission overtook admitted terminal input: entered=%t err=%v", entered, err)
	}
	release()
	if err := f.manager.WithOrchestratorProjectGate(context.Background(), f.op.ProjectID, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestReplacementInputReleaseAdmitsOnlyCurrentOwnerAndLease(t *testing.T) {
	for _, change := range []string{"current", "marker missing", "generation", "capability", "lease"} {
		t.Run(change, func(t *testing.T) {
			f := newReplacementInputTUIFixture(t)
			now := time.Now().UTC()
			f.store.operation.Phase = domain.ReplacementAwaitingSource
			f.store.operation.SourceIntakeReleasedAt = &now
			wrapped := &replacementSourceInputStore{replacementInputTUIStore: f.store}
			f.manager.store = wrapped
			switch change {
			case "marker missing":
				f.store.operation.SourceIntakeReleasedAt = nil
			case "generation", "capability":
				row := f.store.sessions[f.op.SourceSessionID]
				if change == "generation" {
					row.Metadata.ControllerGeneration = "rotated"
				} else {
					row.Metadata.BrowserCapabilityVerifier = "rotated"
				}
				f.store.sessions[row.ID] = row
			case "lease":
				wrapped.fenced = true
			}
			release, err := f.manager.AcquireTerminalInput(context.Background(), f.op.SourceSessionID)
			if release != nil {
				release()
			}
			if change == "current" {
				if err != nil {
					t.Fatal("released source input still denied", err)
				}
			} else if !errors.Is(err, domain.ErrReplacementIntakeFenced) {
				t.Fatal("stale source input admitted", change, err)
			}
			_, finish, err := f.manager.BeginReplacementMutation(context.Background(), f.op.SourceSessionID)
			if finish != nil {
				finish()
			}
			if change == "current" {
				if err != nil {
					t.Fatal("released source chat mutation still denied", err)
				}
			} else if !errors.Is(err, domain.ErrReplacementIntakeFenced) {
				t.Fatal("stale source mutation admitted", change, err)
			}
			_, finish, err = f.manager.beginReplacementSessionAction(context.Background(), f.op.SourceSessionID)
			if finish != nil {
				finish()
			}
			if !errors.Is(err, domain.ErrReplacementInProgress) {
				t.Fatal("source release authorized generic lifecycle mutation", err)
			}
		})
	}
}

type replacementSourceInputStore struct {
	*replacementInputTUIStore
	fenced bool
}

func (s *replacementSourceInputStore) ReplacementWakeFenced(context.Context, domain.SessionID) (bool, error) {
	return s.fenced, nil
}

type disappearingReplacementInputStore struct {
	*replacementSourceInputStore
	reads int
}

func (s *disappearingReplacementInputStore) GetSession(ctx context.Context, id domain.SessionID) (domain.SessionRecord, bool, error) {
	s.reads++
	if s.reads == 2 {
		return domain.SessionRecord{}, false, nil
	}
	return s.replacementSourceInputStore.GetSession(ctx, id)
}
func TestReplacementReleasedInputFailsClosedWhenSourceDisappears(t *testing.T) {
	f := newReplacementInputTUIFixture(t)
	now := time.Now().UTC()
	f.store.operation.Phase = domain.ReplacementAwaitingSource
	f.store.operation.SourceIntakeReleasedAt = &now
	f.manager.store = &disappearingReplacementInputStore{replacementSourceInputStore: &replacementSourceInputStore{replacementInputTUIStore: f.store}}
	release, err := f.manager.AcquireTerminalInput(context.Background(), f.op.SourceSessionID)
	if release != nil {
		release()
	}
	if !errors.Is(err, domain.ErrReplacementSourceChanged) {
		t.Fatal("missing source admitted terminal input", err)
	}
}

// Intake tests need only durable facts and fake pane/runtime boundaries. Native
// transcript parsing and stop/resume producers belong to 3C, not this fixture.
type replacementInputTUIStore struct{ *replacementFenceStore }
type replacementInputTUIFixture struct {
	manager   *Manager
	store     *replacementInputTUIStore
	runtime   *fakeRuntime
	messenger *fakeMessenger
	op        domain.OrchestratorReplacement
}

func newReplacementInputTUIFixture(t *testing.T) replacementInputTUIFixture {
	t.Helper()
	m, base, rt, _ := newManager()
	source := pastStartupGate(mkLive("mer-1"))
	source.Kind, source.Harness, source.Mode, source.Activity.State = domain.KindOrchestrator, domain.HarnessClaudeCode, domain.SessionModeTUI, domain.ActivityIdle
	source.Metadata.RuntimeLaunchID, source.Metadata.AgentSessionIDLaunchID = "source-launch", "source-launch"
	source.Metadata.AgentSessionID, source.Metadata.BrowserCapabilityVerifier = "11111111-1111-4111-8111-111111111111", "source-verifier"
	base.sessions[source.ID] = source
	op := domain.OrchestratorReplacement{ID: "22222222-2222-4222-8222-222222222222", ProjectID: source.ProjectID, SourceSessionID: source.ID, SourceOwner: source.ControllerOwner(), SourceCapabilityVerifier: source.Metadata.BrowserCapabilityVerifier, Phase: domain.ReplacementPreparingHandoff, Revision: 4}
	store := &replacementInputTUIStore{&replacementFenceStore{fakeStore: base, operation: op}}
	messenger := &fakeMessenger{}
	m.store = store
	m.agents = singleAgent{&semanticSignalingAgent{}}
	m.messenger = sessionguard.New(store, messenger, m.logger)
	m.messenger.SetInputLease(m)
	m.messenger.SetStartupSignalGate(m.harnessStartupSignalGatesInput)
	return replacementInputTUIFixture{m, store, rt, messenger, op}
}
