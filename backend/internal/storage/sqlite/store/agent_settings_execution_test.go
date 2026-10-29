package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type settingsExecutionPinner interface {
	PinAgentSettingsExecution(context.Context, domain.SessionID, string, int64, domain.AgentSettingsExecution, time.Time) (domain.AgentSettingsOperation, error)
}

func TestAgentSettingsExecutionRepinsQueuedGenerationWithoutRewritingIntent(t *testing.T) {
	s, db, rec := settingsStartFixture(t, "")
	pinner, ok := any(s).(settingsExecutionPinner)
	if !ok {
		t.Fatal("settings journal has no durable execution preimage")
	}
	op, err := s.BeginAgentSettingsOperation(t.Context(), settingsOperation(rec.ID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE sessions SET controller_generation='replacement' WHERE id=?", rec.ID); err != nil {
		t.Fatal(err)
	}
	current, err := s.SessionAgentSettings(t.Context(), rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	frame := dispatchSnapshot(t, current)
	execution := domain.AgentSettingsExecution{Owner: *frame.Owner, Revision: current.Revision, Qualification: domain.AgentSettingsQualification{InputFingerprint: "binary-account-proof", PolicyVersion: "policy-1"}, Plan: domain.AgentSettingsProviderPlan{Protocol: "acp", ProviderConversationID: current.ProviderConversationID, Source: op.Source, Target: op.Target, Before: domain.AgentSettingsNativeValues{Model: "old-model", Effort: "high", Fast: "off"}, Permission: "default"}}
	pinned, err := pinner.PinAgentSettingsExecution(t.Context(), rec.ID, op.ID, op.Version, execution, time.Now())
	if err != nil || pinned.Execution == nil || pinned.ControllerGeneration != "generation-1" || pinned.Execution.Owner.ControllerGeneration != "replacement" {
		t.Fatalf("execution repin changed original intent: %+v %v", pinned, err)
	}
	raw, _ := json.Marshal(pinned)
	if strings.Contains(string(raw), "binary-account-proof") {
		t.Fatalf("execution proof leaked into public operation: %s", raw)
	}
	if _, err := pinner.PinAgentSettingsExecution(t.Context(), rec.ID, op.ID, op.Version, execution, time.Now()); !errors.Is(err, ports.ErrAgentSettingsConflict) {
		t.Fatalf("stale pin authorized second writer: %v", err)
	}
	op, err = s.AdvanceAgentSettingsOperation(t.Context(), rec.ID, op.ID, pinned.Version, domain.AgentSettingsVerifying, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	op, err = s.AdvanceAgentSettingsOperation(t.Context(), rec.ID, op.ID, op.Version, domain.AgentSettingsVerified, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	op, err = s.CommitAgentSettingsOperation(t.Context(), rec.ID, op.ID, time.Now())
	if err != nil || op.Phase != domain.AgentSettingsApplied {
		t.Fatalf("current executor could not commit: %+v %v", op, err)
	}
}

func TestAgentSettingsExecutionRejectsChangedIdentityAndUnrecoverableOwners(t *testing.T) {
	for _, change := range []string{"account", "native", "branch", "tui"} {
		t.Run(change, func(t *testing.T) {
			s, db, rec := settingsStartFixture(t, "")
			pinner, ok := any(s).(settingsExecutionPinner)
			if !ok {
				t.Fatal("settings journal has no durable execution preimage")
			}
			op, err := s.BeginAgentSettingsOperation(t.Context(), settingsOperation(rec.ID))
			if err != nil {
				t.Fatal(err)
			}
			current, _ := s.SessionAgentSettings(t.Context(), rec.ID)
			frame := dispatchSnapshot(t, current)
			execution := domain.AgentSettingsExecution{Owner: *frame.Owner, Revision: current.Revision, Qualification: domain.AgentSettingsQualification{InputFingerprint: "inputs", PolicyVersion: "policy"}, Plan: domain.AgentSettingsProviderPlan{Protocol: "acp", ProviderConversationID: current.ProviderConversationID, Source: op.Source, Target: op.Target, Before: domain.AgentSettingsNativeValues{Model: "old-model"}}}
			switch change {
			case "account":
				execution.Owner.AccountID = "different-account"
			case "native":
				execution.Owner.ProviderConversationID = "different-native"
			case "branch":
				execution.Owner.BranchID = "different-branch"
			case "tui":
				if _, err := db.Exec("UPDATE sessions SET session_mode='tui',runtime_launch_id=controller_generation WHERE id=?", rec.ID); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := pinner.PinAgentSettingsExecution(t.Context(), rec.ID, op.ID, op.Version, execution, time.Now()); !errors.Is(err, ports.ErrAgentSettingsConflict) {
				t.Fatalf("changed %s authorized: %v", change, err)
			}
			queued, err := s.AgentSettingsOperation(t.Context(), rec.ID, op.ID)
			if err != nil || queued.StartedAt != nil || queued.Version != op.Version || queued.Execution != nil {
				t.Fatalf("rejected pin changed journal: %+v %v", queued, err)
			}
		})
	}
}
