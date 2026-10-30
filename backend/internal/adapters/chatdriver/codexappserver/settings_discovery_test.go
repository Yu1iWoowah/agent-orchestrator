package codexappserver

import (
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestTUISettingsDiscoveryResolvesBaselineWithoutOpeningNativeThread(t *testing.T) {
	d, srv := newTestDriver(t)
	srv.reply("config/read", `{"config":{"model":"baseline","model_reasoning_effort":"low","service_tier":"native-fast"}}`)
	srv.reply("model/list", `{"data":[{"id":"old","defaultReasoningEffort":"high","supportedReasoningEfforts":[{"reasoningEffort":"high"}],"serviceTiers":[{"id":"catalog-fast","name":"Fast"}]},{"id":"baseline","isDefault":true,"defaultReasoningEffort":"low","supportedReasoningEfforts":[{"reasoningEffort":"low"}]}]}`)
	source := domain.AgentSettings{Model: "old", Effort: "high", FastMode: domain.AgentFastOff}
	target := domain.AgentSettings{FastMode: domain.AgentFastDefault}
	plan, err := d.DiscoverAgentSettingsPlan(t.Context(), t.TempDir(), nil, source, target)
	if err != nil {
		t.Fatal(err)
	}
	if plan.After.Model != "baseline" || plan.After.Effort != "low" || plan.After.Fast != "native-fast" || plan.Before.Fast != "default" {
		t.Fatalf("discovery omitted reset or guessed baseline: %+v", plan)
	}
	if !srv.sentMethod("config/read") || !srv.sentMethod("model/list") {
		t.Fatal("baseline was not provider-qualified")
	}
	for _, method := range []string{"thread/start", "thread/resume", "turn/start"} {
		if srv.sentMethod(method) {
			t.Fatalf("read-only TUI preflight invoked %s", method)
		}
	}
}
