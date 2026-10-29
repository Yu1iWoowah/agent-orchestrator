package agentsettings

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
)

func TestSettingsRequestCancellationHasDistinctCode(t *testing.T) {
	for _, tc := range []struct {
		cause error
		code  string
	}{{context.Canceled, "AGENT_SETTINGS_REQUEST_CANCELLED"}, {context.DeadlineExceeded, "AGENT_SETTINGS_REQUEST_TIMEOUT"}} {
		t.Run(tc.code, func(t *testing.T) {
			err := settingsError(fmt.Errorf("private diagnostic: %w", tc.cause))
			var got *apierr.Error
			if !errors.As(err, &got) || got.Code != tc.code || got.Kind == apierr.KindInternal {
				t.Fatalf("request outcome = %#v; want distinct non-internal %s", got, tc.code)
			}
		})
	}
}

type restartAdmissionRuntime struct{ durableOnlyRuntime }

func (r restartAdmissionRuntime) QueueTUIAgentSettings(ctx context.Context, op domain.AgentSettingsOperation) (domain.AgentSettingsOperation, error) {
	return r.st.BeginAgentSettingsOperation(ctx, op)
}
func (r restartAdmissionRuntime) CancelTUIAgentSettings(ctx context.Context, id domain.SessionID, operationID string, version int64) (domain.AgentSettingsOperation, error) {
	return r.CancelAgentSettings(ctx, id, operationID, version)
}

func TestTUISettingsExplicitRestartAdmission(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprint(restart), func(t *testing.T) {
			c, st, rec := controlFixture(t)
			c = NewControl(c.core, c.store, c.authorizer, restartAdmissionRuntime{durableOnlyRuntime{st}}, c.now)
			rec, err := st.CreateSession(t.Context(), domain.SessionRecord{ProjectID: rec.ProjectID, Kind: domain.KindWorker, Harness: domain.HarnessCodex, Mode: domain.SessionModeTUI, Metadata: domain.SessionMetadata{Model: "native", RuntimeHandleID: "runtime", RuntimeLaunchID: "source-launch", AgentSessionID: "native-thread", AgentSessionIDLaunchID: "source-launch"}, CreatedAt: c.now(), UpdatedAt: c.now()})
			if err != nil {
				t.Fatal(err)
			}
			if err := st.BindSessionAgentAccount(t.Context(), rec.ID, domain.AgentAccountProviderCodex, "device-codex", c.now()); err != nil {
				t.Fatal(err)
			}
			zero := int64(0)
			high := domain.AgentEffortSelection("high")
			in := Change{ExpectedRevision: &zero, IdempotencyKey: "restart", Settings: domain.AgentSettingsPatch{Effort: &high}}
			if restart {
				in.ApplyMode = domain.AgentSettingsRestart
			}
			op, err := c.Change(t.Context(), Caller{}, rec.ID, in)
			if !restart {
				if !hasCode(err, "AGENT_SETTINGS_RESTART_REQUIRED") {
					t.Fatalf("implicit restart = %+v %v", op, err)
				}
				if _, found, err := st.PendingAgentSettingsOperation(t.Context(), rec.ID); err != nil || found {
					t.Fatalf("implicit restart created intention: %v %v", found, err)
				}
				return
			}
			if err != nil || op.Phase != domain.AgentSettingsPending || op.ApplyMode != domain.AgentSettingsRestart {
				t.Fatalf("explicit restart admission = %+v %v", op, err)
			}
			current, err := st.SessionAgentSettings(t.Context(), rec.ID)
			if err != nil || current.Revision != 0 || current.Settings.Effort != "" {
				t.Fatalf("admission prematurely applied settings: %+v %v", current, err)
			}
		})
	}
}

func TestSettingsCancelledRequestUsesPublicControlBoundary(t *testing.T) {
	c, _, rec := controlFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	zero := int64(0)
	high := domain.AgentEffortSelection("high")
	_, err := c.Change(ctx, Caller{}, rec.ID, Change{ExpectedRevision: &zero, IdempotencyKey: "cancelled-request", Settings: domain.AgentSettingsPatch{Effort: &high}})
	if !hasCode(err, "AGENT_SETTINGS_REQUEST_CANCELLED") {
		t.Fatalf("public control lost request cancellation code: %T %v", err, err)
	}
}

func TestSettingsRestorationValidatesDurablePreferenceNotRetiredTarget(t *testing.T) {
	c, st, rec := controlFixture(t)
	current, err := st.SessionAgentSettings(t.Context(), rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	op := domain.AgentSettingsOperation{SessionID: rec.ID, Source: current.Settings, Target: domain.AgentSettings{Model: "retired-model", FastMode: domain.AgentFastOn}, Actor: domain.AgentSettingsActor{Kind: domain.AgentSettingsActorLocal}}
	owner := domain.AgentSettingsOwner{SessionID: rec.ID, AccountID: current.AccountID, ControllerGeneration: current.OwnerGeneration, ProviderConversationID: current.ProviderConversationID, BranchID: current.BranchID}
	proof, err := c.ValidateAgentSettings(t.Context(), op, owner, true)
	if err != nil || proof.InputFingerprint != "current" {
		t.Fatalf("valid source restoration depended on failed target: %+v %v", proof, err)
	}
	if _, err := c.ValidateAgentSettings(t.Context(), op, owner, false); err == nil {
		t.Fatal("new mutation unexpectedly accepted retired target")
	}
}
