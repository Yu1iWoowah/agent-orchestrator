package codexappserver

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestSpawnFastUsesCatalogTier(t *testing.T) {
	for _, resume := range []bool{false, true} {
		t.Run(map[bool]string{false: "start", true: "resume"}[resume], func(t *testing.T) {
			d, srv := newTestDriver(t)
			srv.respondTo("model/list", `{"data":[{"id":"gpt-test","isDefault":true,"serviceTiers":[{"id":"accelerated","name":"Fast"}]}]}`)
			var conv ports.ChatConversation
			var err error
			method := "thread/start"
			if resume {
				method = "thread/resume"
				conv, err = d.Resume(context.Background(), ports.ChatResumeConfig{WorkspacePath: t.TempDir(), ProviderConversationID: "thread-1", Model: "gpt-test", FastMode: domain.AgentFastOn})
			} else {
				conv, err = d.Start(context.Background(), ports.ChatStartConfig{WorkspacePath: t.TempDir(), Model: "gpt-test", FastMode: domain.AgentFastOn})
			}
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conv.Close() }()
			request := srv.awaitFrame(func(f frame) bool { return f.Method == method })
			var params struct {
				Config map[string]any `json:"config"`
			}
			if err := json.Unmarshal(request.Params, &params); err != nil {
				t.Fatal(err)
			}
			if params.Config["service_tier"] != "accelerated" {
				t.Fatalf("%s config=%v; want catalog tier accelerated", method, params.Config)
			}
		})
	}
}

func TestResumeFastOffOverridesPaidThreadTier(t *testing.T) {
	d, srv := newTestDriver(t)
	srv.respondTo("thread/resume", `{"thread":{"id":"thread-1"},"serviceTier":"priority"}`)
	conv, err := d.Resume(context.Background(), ports.ChatResumeConfig{WorkspacePath: t.TempDir(), ProviderConversationID: "thread-1", FastMode: domain.AgentFastOff})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conv.Close() }()
	request := srv.awaitFrame(func(f frame) bool { return f.Method == "thread/resume" })
	var params struct {
		Config map[string]any `json:"config"`
	}
	if err := json.Unmarshal(request.Params, &params); err != nil {
		t.Fatal(err)
	}
	if params.Config["service_tier"] != "default" {
		t.Fatalf("off resumed paid tier: %v", params.Config)
	}
}

func TestSpawnFastUsesThreadLocalConfig(t *testing.T) {
	for _, tc := range []struct{ mode, tier string }{{"on", "priority"}, {"off", "default"}, {"default", ""}} {
		t.Run(tc.mode, func(t *testing.T) {
			d, srv := newTestDriver(t)
			cfg := ports.ChatStartConfig{WorkspacePath: t.TempDir()}
			if err := json.Unmarshal([]byte(`{"FastMode":"`+tc.mode+`"}`), &cfg); err != nil {
				t.Fatal(err)
			}
			conv, err := d.Start(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conv.Close() }()
			request := srv.awaitFrame(func(f frame) bool { return f.Method == "thread/start" })
			var params struct {
				Config map[string]any `json:"config"`
			}
			if err = json.Unmarshal(request.Params, &params); err != nil {
				t.Fatal(err)
			}
			got, present := params.Config["service_tier"]
			if tc.tier == "" {
				if present {
					t.Fatalf("default wrote config=%v", params.Config)
				}
				return
			}
			if got != tc.tier {
				t.Fatalf("thread config=%v want tier=%s", params.Config, tc.tier)
			}
		})
	}
}
