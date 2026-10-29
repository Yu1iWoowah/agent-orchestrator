package store_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestAccountRuntimeReceiptRequiresExactGenerationAndConfirmedStop(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	worker := seedUsageSession(t, s, domain.HarnessClaudeCode)
	now := time.Now().UTC()
	created, _, err := s.CreateSessionInterfaceTransition(ctx, domain.SessionInterfaceTransition{ID: "receipt-C", SessionID: worker.ID, SourceMode: domain.SessionModeTUI, TargetMode: domain.SessionModeTUI, Policy: domain.SessionInterfaceTransitionDrain, HistoryPolicy: domain.SessionInterfaceTransitionHistoryStrict, Phase: domain.SessionInterfaceTransitionTargetStarting, SourceAccountID: "A", TargetAccountID: "B", CreatedAt: now, UpdatedAt: now})
	mustNoError(t, err)
	if created.TargetRuntimeState != domain.AccountTransitionRuntimeNotStarted {
		t.Fatalf("new intent is not positive no-effect evidence: %+v", created)
	}
	steps := []struct {
		state          domain.AccountTransitionRuntimeState
		launch, handle string
		want           bool
	}{
		{domain.AccountTransitionRuntimeStarted, "target-1", "opaque-handle", false},
		{domain.AccountTransitionRuntimeReserved, "target-1", "", true},
		{domain.AccountTransitionRuntimeStarted, "wrong-generation", "opaque-handle", false},
		{domain.AccountTransitionRuntimeStarted, "target-1", "opaque-handle", true},
		{domain.AccountTransitionRuntimeReserved, "second-writer", "", false},
		{domain.AccountTransitionRuntimeStopped, "target-1", "wrong-handle", false},
		{domain.AccountTransitionRuntimeStopped, "target-1", "opaque-handle", true},
		{domain.AccountTransitionRuntimeReserved, "target-2", "", true},
	}
	for _, step := range steps {
		changed, err := s.RecordAccountTransitionRuntime(ctx, created.ID, step.state, step.launch, step.handle, now)
		mustNoError(t, err)
		if changed != step.want {
			t.Fatalf("unsafe receipt update: %+v changed=%t", step, changed)
		}
	}
	row, _, err := s.GetSessionInterfaceTransition(ctx, created.ID)
	mustNoError(t, err)
	encoded, err := json.Marshal(row)
	mustNoError(t, err)
	if strings.Contains(string(encoded), "target-2") || strings.Contains(string(encoded), "Runtime") {
		t.Fatal("runtime receipt leaked into API projection")
	}
	changed, err := s.AdvanceSessionInterfaceTransition(ctx, created.ID, domain.SessionInterfaceTransitionTargetStarting, domain.SessionInterfaceTransitionFailed, "", "safe-test-stop", "", now)
	mustNoError(t, err)
	if !changed {
		t.Fatal("fixture did not release active slot")
	}
	second := created
	second.ID = "receipt-race"
	_, _, err = s.CreateSessionInterfaceTransition(ctx, second)
	mustNoError(t, err)
	var wins atomic.Int32
	var wg sync.WaitGroup
	for _, generation := range []string{"race-1", "race-2"} {
		wg.Go(func() {
			ok, err := s.RecordAccountTransitionRuntime(ctx, second.ID, domain.AccountTransitionRuntimeReserved, generation, "", now)
			if err != nil {
				t.Error(err)
			}
			if ok {
				wins.Add(1)
			}
		})
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("two target generations reserved ownership: %d", wins.Load())
	}
}
