package codexappserver

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestJournalledCodexResetSendsResolvedBaselineAndExplicitStandard(t *testing.T) {
	for _, fast := range []domain.AgentFastMode{domain.AgentFastOff, domain.AgentFastDefault, domain.AgentFastOn} {
		t.Run(string(fast), func(t *testing.T) {
			d, srv := newTestDriver(t)
			srv.reply("config/read", `{"config":{"model":"baseline","model_reasoning_effort":"low","service_tier":"native-fast"}}`)
			srv.reply("model/list", `{"data":[{"id":"baseline","isDefault":false,"defaultReasoningEffort":"medium","supportedReasoningEfforts":[{"reasoningEffort":"low"},{"reasoningEffort":"medium"}],"serviceTiers":[{"id":"catalog-fast","name":"Fast"}]},{"id":"old","isDefault":true,"defaultReasoningEffort":"high","serviceTiers":[{"id":"old-fast","name":"Fast"}]}]}`)
			c, err := d.Resume(t.Context(), ports.ChatResumeConfig{ProviderConversationID: "thread-1", WorkspacePath: t.TempDir(), Model: "old", Effort: "high", FastMode: domain.AgentFastOn})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = c.Close() })
			staged, ok := c.(ports.ChatAgentSettingsController)
			if !ok {
				t.Fatal("Codex has no strict journalled reset executor")
			}
			source := domain.AgentSettings{Model: "old", Effort: "high", FastMode: domain.AgentFastOn}
			target := domain.AgentSettings{FastMode: fast}
			plan, err := staged.PrepareAgentSettings(t.Context(), source, target)
			if err != nil {
				t.Fatal(err)
			}
			for _, step := range []domain.AgentSettingsCheckpoint{domain.AgentSettingsApplyingModel, domain.AgentSettingsApplyingEffort, domain.AgentSettingsApplyingFast, domain.AgentSettingsVerifying} {
				if err := staged.ApplyAgentSettingsStep(t.Context(), plan, step, false); err != nil {
					t.Fatalf("%s: %v", step, err)
				}
			}
			if _, err := c.SendTurn(t.Context(), ports.ChatUserMessage{Text: "after reset", Settings: ports.ChatTurnSettings{FastMode: fast}}); err != nil {
				t.Fatal(err)
			}
			f := srv.awaitFrame(func(f frame) bool { return f.Method == "turn/start" })
			var params map[string]any
			if err := json.Unmarshal(f.Params, &params); err != nil {
				t.Fatal(err)
			}
			tier := "default"
			if fast == domain.AgentFastDefault {
				tier = "native-fast"
			}
			if fast == domain.AgentFastOn {
				tier = "catalog-fast"
			}
			if params["model"] != "baseline" || params["effort"] != "low" || params["serviceTier"] != tier {
				t.Fatalf("sticky reset or guessed tier: %+v", params)
			}
		})
	}
}

func TestJournalledCodexResetRejectsUnverifiedBaseline(t *testing.T) {
	d, srv := newTestDriver(t)
	c, err := d.Start(t.Context(), ports.ChatStartConfig{WorkspacePath: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	srv.replyError("config/read", -32603, "baseline unavailable")
	staged, ok := c.(ports.ChatAgentSettingsController)
	if !ok {
		t.Fatal("Codex has no strict journalled reset executor")
	}
	_, err = staged.PrepareAgentSettings(t.Context(), domain.AgentSettings{Model: "old", Effort: "high", FastMode: domain.AgentFastOn}, domain.AgentSettings{FastMode: domain.AgentFastOff})
	if !errors.Is(err, ports.ErrAgentSettingsCapabilitiesUnavailable) {
		t.Fatalf("unverified reset accepted: %v", err)
	}
}

func TestCodexRecoveryBaselineDoesNotNeedTheFailedTargetsOldModel(t *testing.T) {
	d, srv := newTestDriver(t)
	c, err := d.Start(t.Context(), ports.ChatStartConfig{WorkspacePath: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	srv.reply("config/read", `{"config":{"model":"new-baseline","model_reasoning_effort":"low","service_tier":"native-fast"}}`)
	srv.reply("model/list", `{"data":[{"id":"new-baseline","isDefault":true,"defaultReasoningEffort":"low"}]}`)
	provider, ok := c.(ports.ChatAgentSettingsRecoveryPreparer)
	if !ok {
		t.Fatal("Codex cannot prove fresh recovery wire")
	}
	current := domain.AgentSettings{FastMode: domain.AgentFastOff}
	plan, err := provider.PrepareAgentSettingsRecovery(t.Context(), current)
	if err != nil {
		t.Fatal(err)
	}
	staged := c.(ports.ChatAgentSettingsController)
	if err := staged.ApplyAgentSettingsStep(t.Context(), plan, domain.AgentSettingsRollingBack, true); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SendTurn(t.Context(), ports.ChatUserMessage{Text: "after input change", Settings: ports.ChatTurnSettings{FastMode: domain.AgentFastOff}}); err != nil {
		t.Fatal(err)
	}
	f := srv.awaitFrame(func(f frame) bool { return f.Method == "turn/start" })
	var params map[string]any
	if err := json.Unmarshal(f.Params, &params); err != nil {
		t.Fatal(err)
	}
	if params["model"] != "new-baseline" || params["effort"] != "low" || params["serviceTier"] != "default" {
		t.Fatalf("recovery used stale native input or enabled paid tier: %+v", params)
	}
}
