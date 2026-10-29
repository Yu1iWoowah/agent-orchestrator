package chat_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/agentsettings"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

type changingSettingsInputs struct{ changed atomic.Bool }

func (c *changingSettingsInputs) CatalogFingerprint(context.Context, string, string) (string, error) {
	if c.changed.Load() {
		return "changed", nil
	}
	return "original", nil
}

func TestManagedPendingRechecksAuthorityPolicyAndInputsBeforeSetters(t *testing.T) {
	for _, reason := range []string{"expiry", "generation", "policy", "binary"} {
		t.Run(reason, func(t *testing.T) {
			h, conv, _, _ := journalHarness(t)
			ctx := t.Context()
			record, _, err := h.st.GetSession(ctx, testSession)
			if err != nil {
				t.Fatal(err)
			}
			record.Kind = domain.KindWorker
			if err := h.st.UpdateSession(ctx, record); err != nil {
				t.Fatal(err)
			}
			project, _, err := h.st.GetProject(ctx, string(record.ProjectID))
			if err != nil {
				t.Fatal(err)
			}
			project.Config.AgentSettingsPolicy = &domain.AgentSettingsPolicy{Enabled: true, Harnesses: map[domain.AgentHarness]domain.AgentSettingsAllowance{domain.HarnessCodex: {Models: []domain.AgentModelSelection{"next"}, Efforts: []domain.AgentEffortSelection{"high"}}}}
			if err := h.st.UpsertProject(ctx, project); err != nil {
				t.Fatal(err)
			}
			actor, err := h.st.CreateSession(ctx, domain.SessionRecord{ProjectID: record.ProjectID, Kind: domain.KindOrchestrator, Harness: domain.HarnessCodex, Mode: domain.SessionModeTUI, Metadata: domain.SessionMetadata{RuntimeLaunchID: "actor-gen"}, CreatedAt: h.now(), UpdatedAt: h.now()})
			if err != nil {
				t.Fatal(err)
			}
			signer, err := agentsettings.NewGrantAuthority([]byte(strings.Repeat("s", 32)), h.now)
			if err != nil {
				t.Fatal(err)
			}
			scope := domain.AgentSettingsAuthority{ProjectID: record.ProjectID, ActorHarness: domain.HarnessCodex, ActorMode: domain.SessionModeTUI, ActorGeneration: "actor-gen", IssuedAt: h.now().Unix(), ExpiresAt: h.now().Add(time.Hour).Unix()}
			token, err := signer.Issue(actor.ID, scope)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(ports.AgentModelCatalog{AgentID: "codex", Models: []ports.AgentModelInfo{{ID: "next", IsDefault: true, Efforts: []string{"high"}, SupportsFast: true}}})
			if err := h.st.UpsertAgentModelCatalog(ctx, ports.CachedAgentModelCatalog{AgentID: "codex", CatalogJSON: string(raw), LastSuccessAt: h.now(), FetchedAt: h.now(), InputFingerprint: "original"}); err != nil {
				t.Fatal(err)
			}
			inputs := &changingSettingsInputs{}
			core := agentsettings.New(h.st, h.now)
			core.SetCatalogFingerprintSource(inputs)
			control := agentsettings.NewControl(core, h.st, agentsettings.NewAuthorizer(h.st, signer, h.now), h.svc, h.now)
			h.svc.SetAgentSettingsMutator(control)
			h.svc.SetAgentSettingsValidator(control)
			turn, err := h.ctrl.Send(ctx, ports.ChatUserMessage{Text: "current"})
			if err != nil {
				t.Fatal(err)
			}
			revision := int64(0)
			model := domain.AgentModelSelection("next")
			effort := domain.AgentEffortSelection("high")
			op, err := control.Change(ctx, agentsettings.Caller{SessionID: actor.ID, Grant: token}, testSession, agentsettings.Change{ExpectedRevision: &revision, IdempotencyKey: "managed", Settings: domain.AgentSettingsPatch{Model: &model, Effort: &effort}})
			if err != nil || op.Phase != domain.AgentSettingsPending {
				t.Fatalf("accept %+v %v", op, err)
			}
			want := ""
			switch reason {
			case "expiry":
				h.advance(time.Hour)
				want = "AGENT_SETTINGS_GRANT_EXPIRED"
			case "generation":
				actor.Metadata.RuntimeLaunchID = "replacement"
				if err := h.st.UpdateSession(ctx, actor); err != nil {
					t.Fatal(err)
				}
				want = "AGENT_SETTINGS_ACTOR_DENIED"
			case "policy":
				project.Config.AgentSettingsPolicy.Enabled = false
				if err := h.st.UpsertProject(ctx, project); err != nil {
					t.Fatal(err)
				}
				want = "AGENT_SETTINGS_POLICY_DENIED"
			case "binary":
				inputs.changed.Store(true)
				want = "AGENT_SETTINGS_CAPABILITIES_UNAVAILABLE"
			}
			conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: turn.ProviderTurnID, TurnState: domain.TurnStateCompleted})
			h.awaitSnapshot(t, func(_ store.ConversationSnapshot) bool {
				saved, e := h.st.AgentSettingsOperation(ctx, testSession, op.ID)
				return e == nil && saved.Phase == domain.AgentSettingsFailed
			})
			failed, err := h.st.AgentSettingsOperation(ctx, testSession, op.ID)
			if err != nil || failed.ErrorCode != want || conv.stepCount() != 0 {
				t.Fatalf("revoked operation %+v %v setters=%d", failed, err, conv.stepCount())
			}
			current, err := h.st.SessionAgentSettings(ctx, testSession)
			if err != nil || current.Revision != 0 || current.Settings != op.Source {
				t.Fatal("rejected pending mutated durable preference")
			}
			if _, found, err := h.st.PendingAgentSettingsOperation(ctx, testSession); err != nil || found {
				t.Fatal("rejected pending kept its slot")
			}
		})
	}
}
