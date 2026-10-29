package cli

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

const projectConfigFixture = `{
	"orchestratorHandoff":{"sharedDir":"shared","documentPath":"handoff/current.md"},
	"canonicalRepoURL":"https://github.com/acme/demo",
	"defaultBranch":"main","sessionPrefix":"demo",
	"env":{"EDITOR":"vi"},"symlinks":[".env"],
	"postCreate":["echo created"],"preRemove":["echo removing"],
	"agentRules":"Run tests.","agentRulesFile":"rules.md","orchestratorRules":"Coordinate.",
	"agentConfig":{"model":"default-model","effort":"medium","mode":"high","permissions":"auto"},
	"worker":{"agent":"codex","agentConfig":{"model":"worker-model","effort":"high","permissions":"accept-edits"}},
	"orchestrator":{"agent":"codex","agentConfig":{"model":"orchestrator-model","effort":"low"}},
	"reviewers":[{"harness":"codex","agentConfig":{"model":"reviewer-model","effort":"xhigh"}}],
	"trackerIntake":{"enabled":true,"provider":"github","repo":"acme/demo","assignee":"alice"},
	"containerReap":{"disabled":true},"autoReview":true,"workersRequestReview":true
}`

func TestProjectGet_ConfigFields(t *testing.T) {
	for _, format := range []string{"json", "text"} {
		t.Run(format, func(t *testing.T) {
			cfg := setConfigEnv(t)
			srv, _ := projectServer(t, http.StatusOK, `{"status":"ok","project":{"id":"demo","config":`+projectConfigFixture+`}}`)
			writeRunFileFor(t, cfg, srv)
			args := []string{"project", "get", "demo"}
			if format == "json" {
				args = append(args, "--json")
			}
			out, errOut, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, args...)
			if err != nil {
				t.Fatalf("get: %v\nstderr=%s", err, errOut)
			}
			var config json.RawMessage
			if format == "json" {
				var res struct {
					Project struct {
						Config json.RawMessage `json:"config"`
					} `json:"project"`
				}
				if err := json.Unmarshal([]byte(out), &res); err != nil {
					t.Fatalf("decode get output: %v", err)
				}
				config = res.Project.Config
			} else {
				_, value, found := strings.Cut(out, "  config: ")
				if !found {
					t.Fatalf("missing config row: %s", out)
				}
				line, _, _ := strings.Cut(value, "\n")
				config = json.RawMessage(line)
			}
			assertProjectConfigPreserved(t, config, projectConfigFixture)
		})
	}
}

func TestProjectSetConfig_PreservesConfigFields(t *testing.T) {
	for _, source := range []string{"config-json", "get-edit-write"} {
		t.Run(source, func(t *testing.T) {
			cfg := setConfigEnv(t)
			srv, capture := projectServer(t, http.StatusOK, `{"project":{"id":"demo","config":`+projectConfigFixture+`}}`)
			writeRunFileFor(t, cfg, srv)
			deps := Deps{ProcessAlive: func(int) bool { return true }}
			input := projectConfigFixture
			if source == "get-edit-write" {
				out, errOut, err := executeCLI(t, deps, "project", "get", "demo", "--json")
				if err != nil {
					t.Fatalf("get: %v\nstderr=%s", err, errOut)
				}
				var res struct {
					Project struct {
						Config map[string]json.RawMessage `json:"config"`
					} `json:"project"`
				}
				if err := json.Unmarshal([]byte(out), &res); err != nil {
					t.Fatalf("decode get output: %v", err)
				}
				res.Project.Config["sessionPrefix"] = json.RawMessage(`"edited"`)
				data, err := json.Marshal(res.Project.Config)
				if err != nil {
					t.Fatal(err)
				}
				input = string(data)
			}
			out, errOut, err := executeCLI(t, deps, "project", "set-config", "demo", "--config-json", input, "--json")
			if err != nil {
				t.Fatalf("set-config: %v\nstderr=%s", err, errOut)
			}
			if capture.method != http.MethodPut || capture.path != "/api/v1/projects/demo/config" {
				t.Fatalf("request = %s %s, want PUT /api/v1/projects/demo/config", capture.method, capture.path)
			}
			var req struct {
				Config json.RawMessage `json:"config"`
			}
			if err := json.Unmarshal(capture.body, &req); err != nil {
				t.Fatalf("decode PUT body: %v", err)
			}
			want := projectConfigFixture
			if source == "get-edit-write" {
				want = strings.Replace(want, `"sessionPrefix":"demo"`, `"sessionPrefix":"edited"`, 1)
			}
			assertProjectConfigPreserved(t, req.Config, want)
			var res struct {
				Project struct {
					Config json.RawMessage `json:"config"`
				} `json:"project"`
			}
			if err := json.Unmarshal([]byte(out), &res); err != nil {
				t.Fatalf("decode set-config output: %v", err)
			}
			assertProjectConfigPreserved(t, res.Project.Config, projectConfigFixture)
		})
	}
}

func assertProjectConfigPreserved(t *testing.T, data []byte, wantJSON string) {
	t.Helper()
	var got, want domain.ProjectConfig
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode config: %v\n%s", err, data)
	}
	if err := json.Unmarshal([]byte(wantJSON), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("config fields lost or changed:\ngot  %#v\nwant %#v", got, want)
	}
}

func TestProjectConfigMirrorsDomainJSONFields(t *testing.T) {
	for _, pair := range []struct {
		cli    any
		domain any
	}{
		{projectConfig{}, domain.ProjectConfig{}},
		{agentSettingsPolicy{}, domain.AgentSettingsPolicy{}},
		{agentSettingsAllowance{}, domain.AgentSettingsAllowance{}},
		{agentConfig{}, domain.AgentConfig{}},
		{roleOverride{}, domain.RoleOverride{}},
		{reviewerConfig{}, domain.ReviewerConfig{}},
		{trackerIntakeConfig{}, domain.TrackerIntakeConfig{}},
		{containerReapConfig{}, domain.ContainerReapConfig{}},
		{orchestratorHandoffConfig{}, domain.OrchestratorHandoffConfig{}},
	} {
		t.Run(reflect.TypeOf(pair.cli).Name(), func(t *testing.T) {
			fields := func(value any) map[string]bool {
				result := make(map[string]bool)
				for _, field := range reflect.VisibleFields(reflect.TypeOf(value)) {
					name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
					if !field.IsExported() || name == "-" {
						continue
					}
					if name == "" {
						name = field.Name
					}
					result[name] = true
				}
				return result
			}
			if got, want := fields(pair.cli), fields(pair.domain); !reflect.DeepEqual(got, want) {
				t.Errorf("CLI JSON fields = %v; domain JSON fields = %v", got, want)
			}
		})
	}
}
