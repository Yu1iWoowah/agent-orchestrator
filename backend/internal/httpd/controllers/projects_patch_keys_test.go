package controllers_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd"
	projectsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/project"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

func TestProjectsAPI_PatchCanonicalKeys(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"agentconfig alias bypass", `{"config":{"worker":{"agentconfig":{"model":"new"}}},"expected":{"config":{"worker":{"agentconfig":{"model":null}}}}}`, 400},
		{"canonical and alias together", `{"config":{"worker":{"agentConfig":{"effort":"low"},"agentconfig":{"model":"new"}}},"expected":{"config":{"worker":{"agentConfig":{"effort":"high"},"agentconfig":{"model":null}}}}}`, 400},
		{"unicode fold alias", `{"config":{"worker":{"agentConfig":{"permi\u017f\u017fions":"accept-edits"}}},"expected":{"config":{"worker":{"agentConfig":{"permi\u017f\u017fions":null}}}}}`, 400},
		{"null alias", `{"config":{"worker":{"agentconfig":null}},"expected":{"config":{"worker":{"agentconfig":null}}}}`, 400},
		{"NUL in member", `{"config":{"worker":{"agentConfig":{"model\u0000":null}}},"expected":{"config":{"worker":{"agentConfig":{"model\u0000":null}}}}}`, 400},
		{"reviewer array alias", `{"config":{"reviewers":[{"harness":"codex","agentconfig":{"model":"new"}}]},"expected":{"config":{"reviewers":[{"harness":"codex","agentConfig":{}}]}}}`, 400},
		{"duplicate escaped canonical key", `{"config":{"worker":{"agentConfig":{"model":"one","mo\u0064el":"two"}}},"expected":{"config":{"worker":{"agentConfig":{"model":"current"}}}}}`, 400},
		{"duplicate root alias", `{"config":{"worker":{"agentConfig":{"model":"one"}}},"Config":{"worker":{"agentConfig":{"model":"two"}}},"expected":{"config":{"worker":{"agentConfig":{"model":"current"}}}}}`, 400},
		{"escaped canonical spelling", `{"config":{"worker":{"agent\u0043onfig":{"model":"new"}}},"expected":{"config":{"worker":{"agentConfig":{"model":"current"}}}}}`, 200},
		{"dynamic env key spelling preserved", `{"config":{"env":{"Mixed_Case":"雪","Other_case":"火"}},"expected":{"config":{"env":{"Mixed_Case":null,"Other_case":null}}}}`, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			st := sqlitetest.MustOpen(t)
			cfg := domain.ProjectConfig{Worker: domain.RoleOverride{AgentConfig: domain.AgentConfig{Model: "current", Effort: "high", Permissions: domain.PermissionModeAuto}}, Reviewers: []domain.ReviewerConfig{{Harness: domain.ReviewerCodex}}}
			if err := st.UpsertProject(ctx, domain.ProjectRecord{ID: "p", Path: t.TempDir(), Config: cfg}); err != nil {
				t.Fatal(err)
			}
			before, _, err := st.GetProject(ctx, "p")
			if err != nil {
				t.Fatal(err)
			}
			log := slog.New(slog.NewTextHandler(io.Discard, nil))
			srv := httptest.NewServer(httpd.NewRouterWithControl(config.Config{}, log, nil, httpd.APIDeps{Projects: projectsvc.New(st)}, httpd.ControlDeps{}))
			t.Cleanup(srv.Close)
			body, status, _ := doRequest(t, srv, http.MethodPatch, "/api/v1/projects/p/settings", tc.body)
			after, _, err := st.GetProject(ctx, "p")
			if err != nil {
				t.Fatal(err)
			}
			if status != tc.status {
				t.Errorf("status=%d want=%d model=%q config=%+v body=%s", status, tc.status, after.Config.Worker.AgentConfig.Model, after.Config, body)
			}
			if tc.status == 400 && !reflect.DeepEqual(before, after) {
				t.Error("invalid canonical keys changed the persisted project")
			}
			if tc.name == "escaped canonical spelling" && after.Config.Worker.AgentConfig.Model != "new" {
				t.Error("canonical JSON escape did not update the intended leaf")
			}
			if tc.name == "dynamic env key spelling preserved" && (after.Config.Env["Mixed_Case"] != "雪" || after.Config.Env["Other_case"] != "火") {
				t.Fatalf("dynamic map keys were folded: %v", after.Config.Env)
			}
		})
	}
}
