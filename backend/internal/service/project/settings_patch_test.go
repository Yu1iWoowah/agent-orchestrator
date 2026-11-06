package project_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/project"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

func patchInput(t *testing.T, body string) project.PatchSettingsInput {
	t.Helper()
	var in project.PatchSettingsInput
	if err := json.Unmarshal([]byte(body), &in); err != nil {
		t.Fatal(err)
	}
	return in
}

func TestPatchSettingsLeaves(t *testing.T) {
	for _, tc := range []struct {
		name, body, code string
		check            func(*testing.T, domain.ProjectRecord)
	}{
		{"worker effort only (A3)", `{"config":{"worker":{"agentConfig":{"effort":"high"}}},"expected":{"config":{"worker":{"agentConfig":{"effort":"low"}}}}}`, "", func(t *testing.T, row domain.ProjectRecord) {
			if row.Config.Worker.AgentConfig.Effort != "high" || row.Config.Worker.AgentConfig.Model != "current" {
				t.Fatalf("worker = %+v", row.Config.Worker)
			}
		}},
		{"other editor's reviewers preserved (A4)", `{"config":{"worker":{"agentConfig":{"model":"new"}}},"expected":{"config":{"worker":{"agentConfig":{"model":"current"}}}}}`, "", func(t *testing.T, row domain.ProjectRecord) {
			if row.Config.Worker.AgentConfig.Model != "new" || row.Config.Reviewers[0].AgentConfig.Model != "external" {
				t.Fatalf("config = %+v", row.Config)
			}
		}},
		{"same leaf conflict rejects entire write (A5)", `{"displayName":"changed","config":{"worker":{"agentConfig":{"model":"new"}}},"expected":{"displayName":"Project","config":{"worker":{"agentConfig":{"model":"stale"}}}}}`, "PROJECT_SETTINGS_CONFLICT", nil},
		{"missing expected", `{"config":{"worker":{"agentConfig":{"model":"new"}}}}`, "PROJECT_SETTINGS_EXPECTED_REQUIRED", nil},
		{"missing expected leaf", `{"config":{"worker":{"agentConfig":{"model":"new","effort":"high"}}},"expected":{"config":{"worker":{"agentConfig":{"model":"current"}}}}}`, "PROJECT_SETTINGS_EXPECTED_REQUIRED", nil},
		{"forbidden key", `{"config":{"agentRules":"new"},"expected":{"config":{"agentRules":null}}}`, "INVALID_PROJECT_CONFIG", nil},
		{"invalid nested type", `{"config":{"worker":{"agentConfig":{"model":42}}},"expected":{"config":{"worker":{"agentConfig":{"model":"current"}}}}}`, "INVALID_PROJECT_CONFIG", nil},
		{"unknown nested key", `{"config":{"worker":{"typo":"x"}},"expected":{"config":{"worker":{"typo":null}}}}`, "INVALID_PROJECT_CONFIG", nil},
		{"existing permission validation", `{"config":{"worker":{"agentConfig":{"permissions":"bad"}}},"expected":{"config":{"worker":{"agentConfig":{"permissions":null}}}}}`, "INVALID_PROJECT_CONFIG", nil},
		{"null deletes one env key", `{"config":{"env":{"REMOVE":null}},"expected":{"config":{"env":{"REMOVE":"value"}}}}`, "", func(t *testing.T, row domain.ProjectRecord) {
			if _, exists := row.Config.Env["REMOVE"]; exists || row.Config.Env["KEEP"] != "value" {
				t.Fatalf("env = %+v", row.Config.Env)
			}
		}},
		{"array replaces", `{"config":{"postCreate":["new"]},"expected":{"config":{"postCreate":["old","other"]}}}`, "", func(t *testing.T, row domain.ProjectRecord) {
			if !reflect.DeepEqual(row.Config.PostCreate, []string{"new"}) {
				t.Fatalf("postCreate = %v", row.Config.PostCreate)
			}
		}},
		{"absent expected is null", `{"config":{"sessionPrefix":"prefix"},"expected":{"config":{"sessionPrefix":null}}}`, "", func(t *testing.T, row domain.ProjectRecord) {
			if row.Config.SessionPrefix != "prefix" {
				t.Fatalf("prefix = %q", row.Config.SessionPrefix)
			}
		}},
		{"display name only", `{"displayName":" New name ","expected":{"displayName":"Project"}}`, "", func(t *testing.T, row domain.ProjectRecord) {
			if row.DisplayName != "New name" {
				t.Fatalf("displayName = %q", row.DisplayName)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st, err := sqlitetest.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			before := domain.ProjectRecord{ID: "p", Path: t.TempDir(), DisplayName: "Project", Config: domain.ProjectConfig{
				DefaultBranch: "main", AgentRules: "keep rules",
				Worker:    domain.RoleOverride{AgentConfig: domain.AgentConfig{Model: "current", Effort: "low"}},
				Reviewers: []domain.ReviewerConfig{{Harness: domain.ReviewerCodex, AgentConfig: domain.AgentConfig{Model: "external"}}},
				Env:       map[string]string{"REMOVE": "value", "KEEP": "value"}, PostCreate: []string{"old", "other"},
			}}
			if err := st.UpsertProject(ctx, before); err != nil {
				t.Fatal(err)
			}
			before, _, _ = st.GetProject(ctx, "p")
			insideGate := false
			svc := project.NewWithDeps(project.Deps{Store: st, WithProjectGate: func(ctx context.Context, id domain.ProjectID, run func(context.Context) error) error {
				if id != "p" {
					t.Fatalf("gate project = %q", id)
				}
				insideGate = true
				return run(ctx)
			}})
			_, err = svc.PatchSettings(ctx, "p", patchInput(t, tc.body))
			after, _, loadErr := st.GetProject(ctx, "p")
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if tc.code != "" {
				wantCode(t, err, tc.code)
				if !reflect.DeepEqual(before, after) {
					t.Fatalf("rejected mutation wrote settings: before=%+v after=%+v", before, after)
				}
				return
			}
			if err != nil || !insideGate {
				t.Fatalf("PatchSettings = %v, gate = %v", err, insideGate)
			}
			if after.Config.AgentRules != "keep rules" {
				t.Fatal("untouched rules changed")
			}
			tc.check(t, after)
		})
	}
}

func TestPatchSettingsLegacyMigration(t *testing.T) {
	for _, role := range []string{"worker", "orchestrator", "reviewers"} {
		t.Run(role, func(t *testing.T) {
			ctx := context.Background()
			st, err := sqlitetest.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			cfg := domain.ProjectConfig{AgentConfig: domain.AgentConfig{Model: "claude-legacy", Mode: "high", Effort: "low", Permissions: domain.PermissionModeAuto},
				Reviewers: []domain.ReviewerConfig{{Harness: domain.ReviewerCodex}, {Harness: domain.ReviewerClaudeCode, AgentConfig: domain.AgentConfig{Model: "explicit"}}}}
			if err := st.UpsertProject(ctx, domain.ProjectRecord{ID: "p", Path: t.TempDir(), Config: cfg}); err != nil {
				t.Fatal(err)
			}
			body := `{"config":{"` + role + `":{"agent":"codex","agentConfig":{"model":null}}},"expected":{"config":{"` + role + `":{"agent":null,"agentConfig":{"model":null}}}}}`
			if role == "reviewers" {
				body = `{"config":{"reviewers":[{"harness":"codex","agentConfig":{"model":"new"}}]},"expected":{"config":{"reviewers":[{"harness":"codex","agentConfig":{}},{"harness":"claude-code","agentConfig":{"model":"explicit"}}]}}}`
			}
			_, err = project.New(st).PatchSettings(ctx, "p", patchInput(t, body))
			if err != nil {
				t.Fatal(err)
			}
			row, _, _ := st.GetProject(ctx, "p")
			if row.Config.AgentConfig.Model != "" || row.Config.AgentConfig.Effort != "low" || row.Config.AgentConfig.Mode != "high" || row.Config.AgentConfig.Permissions != domain.PermissionModeAuto {
				t.Fatalf("legacy = %+v", row.Config.AgentConfig)
			}
			for name, got := range map[string]string{"worker": row.Config.Worker.AgentConfig.Model, "orchestrator": row.Config.Orchestrator.AgentConfig.Model, "reviewers": row.Config.Reviewers[0].AgentConfig.Model} {
				want := "claude-legacy"
				if name == role {
					want = ""
					if role == "reviewers" {
						want = "new"
					}
				}
				if got != want {
					t.Errorf("%s effective model = %q, want %q", name, got, want)
				}
			}
			if role != "reviewers" && row.Config.Reviewers[1].AgentConfig.Model != "explicit" {
				t.Fatal("explicit reviewer changed")
			}
		})
	}
}

func TestPatchSettingsValidationAndRemoteAccounts(t *testing.T) {
	ctx := context.Background()
	st, err := sqlitetest.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := project.New(st)
	row := domain.ProjectRecord{ID: "p", Path: t.TempDir(), Kind: domain.ProjectKindScratch, Config: domain.ProjectConfig{Worker: domain.RoleOverride{AgentAccountID: "local"}}}
	if err := st.UpsertProject(ctx, row); err != nil {
		t.Fatal(err)
	}
	_, err = svc.PatchSettings(ctx, "p", patchInput(t, `{"config":{"defaultBranch":"main"},"expected":{"config":{"defaultBranch":null}}}`))
	wantCode(t, err, "INVALID_PROJECT_CONFIG")
	remote := ports.WithRemoteAccountRequest(ctx)
	_, err = svc.PatchSettings(remote, "p", patchInput(t, `{"config":{"worker":{"agentConfig":{"effort":"high"}}},"expected":{"config":{"worker":{"agentConfig":{"effort":null}}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	got, _, _ := st.GetProject(ctx, "p")
	if got.Config.Worker.AgentAccountID != "local" || got.Config.Worker.AgentConfig.Effort != "high" {
		t.Fatalf("remote mutation = %+v", got.Config)
	}
	_, err = svc.PatchSettings(remote, "p", patchInput(t, `{"config":{"worker":{"agentAccountId":"other"}},"expected":{"config":{"worker":{"agentAccountId":null}}}}`))
	wantCode(t, err, "AGENT_ACCOUNT_LOCAL_ONLY")
	row.Kind = domain.ProjectKindSingleRepo
	row.RepoOriginURL = "https://github.com/owner/repo"
	row.Config.CanonicalRepoURL = "https://gitlab.com/owner/repo"
	if err := st.UpsertProject(ctx, row); err != nil {
		t.Fatal(err)
	}
	_, err = svc.PatchSettings(ctx, "p", patchInput(t, `{"displayName":"changed","expected":{"displayName":"p"}}`))
	wantCode(t, err, "INVALID_PROJECT_CONFIG")
}

func TestPatchSettingsDeleteAbsentRole(t *testing.T) {
	st := sqlitetest.MustOpen(t)
	ctx := t.Context()
	if err := st.UpsertProject(ctx, domain.ProjectRecord{ID: "p", Path: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	svc := project.New(st)
	baseline, err := svc.Get(ctx, "p")
	if err != nil || baseline.Project.Config != nil {
		t.Fatalf("empty config read model = %+v, %v", baseline, err)
	}
	updated, err := svc.PatchSettings(ctx, "p", patchInput(t, `{"config":{"worker":null},"expected":{"config":{"worker":null}}}`))
	if err != nil || updated.Config != nil {
		t.Fatalf("deleting absent role = %+v, %v", updated, err)
	}
}

func TestPatchSettingsConcurrentExpected(t *testing.T) {
	ctx := context.Background()
	st, err := sqlitetest.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.UpsertProject(ctx, domain.ProjectRecord{ID: "p", Path: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	svc := project.New(st)
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, prefix := range []string{"first", "second"} {
		in := patchInput(t, `{"config":{"sessionPrefix":"`+prefix+`"},"expected":{"config":{"sessionPrefix":null}}}`)
		wg.Go(func() {
			_, err := svc.PatchSettings(ctx, "p", in)
			results <- err
		})
	}
	wg.Wait()
	close(results)
	success, conflict := 0, 0
	for err := range results {
		var apiError *apierr.Error
		if err == nil {
			success++
		} else if errors.As(err, &apiError) && apiError.Code == "PROJECT_SETTINGS_CONFLICT" {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("success=%d conflict=%d", success, conflict)
	}
}
