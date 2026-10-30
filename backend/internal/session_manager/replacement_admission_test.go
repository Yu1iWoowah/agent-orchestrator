package sessionmanager

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type replacementFenceStore struct {
	*fakeStore
	operation domain.OrchestratorReplacement
	retired   bool
}

func (s *replacementFenceStore) GetActiveOrchestratorReplacement(_ context.Context, project domain.ProjectID) (domain.OrchestratorReplacement, bool, error) {
	return s.operation, s.operation.ProjectID == project && !s.operation.Phase.Terminal(), nil
}

func TestReplacementAdmissionRefusesLegacyActionsBeforeSideEffects(t *testing.T) {
	for _, action := range []string{"spawn", "kill", "request kill", "restore", "resume", "exit", "legacy retire", "interface switch", "provider switch", "account switch"} {
		t.Run(action, func(t *testing.T) {
			m, st, rt, ws := newManager()
			source := mkLive("mer-1")
			source.Kind = domain.KindOrchestrator
			st.sessions[source.ID] = source
			m.store = &replacementFenceStore{fakeStore: st, operation: domain.OrchestratorReplacement{ID: "operation", ProjectID: source.ProjectID, SourceSessionID: source.ID, Phase: domain.ReplacementExportingTranscript}}
			var err error
			switch action {
			case "spawn":
				_, _, _, err = m.Spawn(ctx, ports.SpawnConfig{ProjectID: source.ProjectID, Kind: domain.KindOrchestrator})
			case "kill":
				_, err = m.Kill(ctx, source.ID)
			case "request kill":
				_, err = m.RequestKill(ctx, source.ID)
			case "restore":
				_, err = m.RestoreWithMode(ctx, source.ID)
			case "resume":
				_, err = m.ResumeAgentWithMode(ctx, source.ID)
			case "exit":
				_, err = m.ExitAgent(ctx, source.ID)
			case "interface switch":
				_, err = m.StartInterfaceTransition(ctx, source.ID, domain.SessionModeChat, domain.SessionInterfaceTransitionDrain, domain.SessionInterfaceTransitionHistoryStrict)
			case "account switch":
				_, err = m.StartAccountTransition(ctx, source.ID, "another-account", domain.SessionInterfaceTransitionInterrupt)
			case "provider switch":
				err = m.SwitchTUIProvider(ctx, source.ID, domain.ProviderSwitchRequest{TargetHarness: domain.HarnessCodex})
			case "legacy retire":
				err = m.RetireForReplacement(ctx, source.ID)
			}
			if !errors.Is(err, domain.ErrReplacementInProgress) {
				t.Fatalf("%s bypassed replacement admission: %v", action, err)
			}
			if rt.created != 0 || rt.destroyed != 0 || len(rt.interrupts) != 0 || ws.createCount != 0 || ws.destroyed != 0 || st.sessions[source.ID].IsTerminated {
				t.Fatalf("%s mutated runtime/workspace/source before refusal", action)
			}
		})
	}
}

func TestOrchestratorProjectGateIsCancellableAndRevokesReleasedContext(t *testing.T) {
	m, _, _, _ := newManager()
	var stale context.Context
	if err := m.WithOrchestratorProjectGate(ctx, "mer", func(locked context.Context) error {
		stale = locked
		return m.WithOrchestratorProjectGate(locked, "mer", func(context.Context) error { return nil })
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.WithOrchestratorProjectGate(ctx, "mer", func(context.Context) error {
		cancelled, cancel := context.WithTimeout(stale, 20*time.Millisecond)
		defer cancel()
		entered := false
		err := m.WithOrchestratorProjectGate(cancelled, "mer", func(context.Context) error { entered = true; return nil })
		if entered || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("released/cancelled context bypassed held gate: entered=%t err=%v", entered, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func (s *replacementFenceStore) GetRetiredOrchestratorReplacementBySource(_ context.Context, id domain.SessionID) (domain.OrchestratorReplacement, bool, error) {
	return s.operation, s.retired && s.operation.SourceSessionID == id, nil
}

func TestReplacementAdmissionRefusesRetiredSourceEvenAfterCompletion(t *testing.T) {
	m, st, rt, ws := newManager()
	source := mkLive("mer-1")
	source.Kind = domain.KindOrchestrator
	source.IsTerminated = true
	st.sessions[source.ID] = source
	m.store = &replacementFenceStore{fakeStore: st, retired: true, operation: domain.OrchestratorReplacement{ID: "completed-operation", ProjectID: source.ProjectID, SourceSessionID: source.ID, Phase: domain.ReplacementCompleted}}
	_, err := m.RestoreWithMode(ctx, source.ID)
	if !errors.Is(err, domain.ErrReplacementSourceRetired) {
		t.Fatalf("completed replacement revived retired source: %v", err)
	}
	if rt.created != 0 || ws.createCount != 0 || !st.sessions[source.ID].IsTerminated {
		t.Fatal("retired source changed before refusal")
	}
}

func TestOrchestratorProjectGateRetainsReservationUntilBorrowedActionReturns(t *testing.T) {
	m, _, _, _ := newManager()
	entered := make(chan struct{})
	finish := make(chan struct{})
	done := make(chan error, 1)
	if err := m.WithOrchestratorProjectGate(ctx, "mer", func(locked context.Context) error {
		go func() {
			done <- m.WithOrchestratorProjectGate(locked, "mer", func(context.Context) error { close(entered); <-finish; return nil })
		}()
		<-entered
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	blocked, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	err := m.WithOrchestratorProjectGate(blocked, "mer", func(context.Context) error {
		return errors.New("other request entered before borrowed action returned")
	})
	close(finish)
	if childErr := <-done; childErr != nil {
		t.Fatal(childErr)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if err := m.WithOrchestratorProjectGate(ctx, "mer", func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
}
