package codexappserver

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

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
