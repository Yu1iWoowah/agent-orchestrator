package store_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestAgentSettingsAndControllerTransitionHaveOneWinner(t *testing.T) {
	for _, order := range []string{"settings_first", "transition_first", "concurrent"} {
		t.Run(order, func(t *testing.T) {
			s, _, rec := settingsFixture(t)
			ctx := context.Background()
			now := time.Now()
			transition := domain.SessionInterfaceTransition{ID: "move", SessionID: rec.ID, SourceMode: domain.SessionModeChat, TargetMode: domain.SessionModeTUI, Policy: domain.SessionInterfaceTransitionDrain, HistoryPolicy: domain.SessionInterfaceTransitionHistoryStrict, Phase: domain.SessionInterfaceTransitionRequested, CreatedAt: now, UpdatedAt: now}
			var settingsErr, moveErr error
			var created bool
			settings := func() { _, settingsErr = s.BeginAgentSettingsOperation(ctx, settingsOperation(rec.ID)) }
			move := func() { _, created, moveErr = s.CreateSessionInterfaceTransition(ctx, transition) }
			switch order {
			case "settings_first":
				settings()
				move()
			case "transition_first":
				move()
				settings()
			default:
				var wg sync.WaitGroup
				wg.Add(2)
				go func() { defer wg.Done(); settings() }()
				go func() { defer wg.Done(); move() }()
				wg.Wait()
			}
			wins := 0
			if settingsErr == nil {
				wins++
			}
			if moveErr == nil && created {
				wins++
			}
			if wins != 1 {
				t.Fatalf("settings=%v transition=%v created=%v", settingsErr, moveErr, created)
			}
			loser := settingsErr
			if loser == nil {
				loser = moveErr
			}
			if !errors.Is(loser, ports.ErrAgentSettingsConflict) {
				t.Fatalf("expected stable operation conflict, got %v", loser)
			}
		})
	}
}
