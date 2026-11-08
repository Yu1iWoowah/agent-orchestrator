package hcg

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	projectsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/project"
)

func TestWorkspaceRegistrationFeedsFreshLaunchPolicy(t *testing.T) {
	st, svc, root, _ := launchFixture(t)
	for k, v := range map[string]string{"GIT_AUTHOR_NAME": "AO Test", "GIT_AUTHOR_EMAIL": "ao@example.com", "GIT_COMMITTER_NAME": "AO Test", "GIT_COMMITTER_EMAIL": "ao@example.com"} {
		t.Setenv(k, v)
	}
	parent := t.TempDir()
	child := filepath.Join(parent, "child")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "--initial-branch=main", child}, {"-C", child, "commit", "--allow-empty", "-m", "fixture"}, {"-C", child, "remote", "add", "origin", "https://example.com/fixture.git"}, {"-C", child, "update-ref", "refs/remotes/origin/main", "HEAD"}, {"-C", child, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git: %s %v", out, err)
		}
	}
	projects := projectsvc.NewWithDeps(projectsvc.Deps{Store: st, ReportingPolicyChanged: svc.ProjectPolicyChanged})
	name := "ws"
	on := true
	if _, err := projects.Add(t.Context(), projectsvc.AddInput{Path: parent, ProjectID: &name, AsWorkspace: true, Config: &domain.ProjectConfig{HCGReporting: domain.HCGReportingConfig{Enabled: &on}}}); err != nil {
		t.Fatal(err)
	}
	rec, err := st.CreateSession(t.Context(), domain.SessionRecord{ProjectID: "ws", Kind: domain.KindWorker, Harness: domain.HarnessCodex, Mode: domain.SessionModeChat, Metadata: domain.SessionMetadata{ProviderConversationID: "native"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ClaimChatControllerGeneration(t.Context(), rec.ID, "ws-launch"); err != nil {
		t.Fatal(err)
	}
	rec, _, err = st.GetSession(t.Context(), rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	n := readyNotice(t, st, root, rec.ID, true)
	svc.ReportingLaunchReady(n)
	waitMetadata(t, svc)
	m, ok := readLaunchManifest(root, string(rec.ID))
	if !ok || m.MountInputs.Project == nil || !*m.MountInputs.Project {
		t.Fatalf("workspace durable policy not inherited by fresh launch: %+v", m)
	}
	v, err := svc.Get(t.Context(), rec.ID)
	if err != nil || !v.EffectiveEnabled || v.Reason == "manifest_unavailable" {
		t.Fatalf("workspace report policy: %+v %v", v, err)
	}
}

type delayedPolicyCommit struct {
	projectsvc.Store
	committed, release chan struct{}
}

func (s delayedPolicyCommit) UpsertProject(ctx context.Context, p domain.ProjectRecord) error {
	if err := s.Store.UpsertProject(ctx, p); err != nil {
		return err
	}
	if v := p.Config.Worker.HCGReporting.Enabled; v != nil && !*v {
		close(s.committed)
		select {
		case <-s.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
func TestPolicyNotificationsCannotReorderCommittedSettings(t *testing.T) {
	for _, writer := range []string{"set", "update", "patch"} {
		t.Run(writer, func(t *testing.T) {
			st, old, root, id := launchFixture(t)
			old.Close()
			on, off := true, false
			p, _, err := st.GetProject(t.Context(), "p")
			if err != nil {
				t.Fatal(err)
			}
			p.Config.Worker.HCGReporting.Enabled = &on
			if err := st.UpsertProject(t.Context(), p); err != nil {
				t.Fatal(err)
			}
			svc := New(t.Context(), root, st)
			t.Cleanup(svc.Close)
			committed, release := make(chan struct{}), make(chan struct{})
			projects := projectsvc.NewWithDeps(projectsvc.Deps{Store: delayedPolicyCommit{Store: st, committed: committed, release: release}, ReportingPolicyChanged: svc.ProjectPolicyChanged})
			cfgOff := p.Config
			cfgOff.Worker.HCGReporting.Enabled = &off
			first := make(chan error, 1)
			go func() {
				_, err := projects.SetConfig(t.Context(), "p", projectsvc.SetConfigInput{Config: cfgOff})
				first <- err
			}()
			<-committed
			second := make(chan error, 1)
			started := make(chan struct{})
			go func() {
				close(started)
				var err error
				switch writer {
				case "set":
					_, err = projects.SetConfig(t.Context(), "p", projectsvc.SetConfigInput{Config: p.Config})
				case "update":
					_, err = projects.UpdateSettings(t.Context(), "p", projectsvc.UpdateSettingsInput{DisplayName: "p", Config: p.Config})
				case "patch":
					_, err = projects.PatchSettings(t.Context(), "p", projectsvc.PatchSettingsInput{Config: map[string]any{"worker": map[string]any{"hcgReporting": map[string]any{"enabled": true}}}, Expected: map[string]any{"config": map[string]any{"worker": map[string]any{"hcgReporting": map[string]any{"enabled": false}}}}})
				}
				second <- err
			}()
			<-started
			// Before the fix the second request completes ahead of the paused callback.
			// Correct serialization keeps it pending until the first notification ends.
			var secondErr error
			finished := false
			select {
			case secondErr = <-second:
				finished = true
			case <-time.After(500 * time.Millisecond):
			}
			close(release)
			if err := <-first; err != nil {
				t.Fatal(err)
			}
			if !finished {
				secondErr = <-second
			}
			if secondErr != nil {
				t.Fatal(secondErr)
			}
			durable, _, err := st.GetProject(t.Context(), "p")
			if err != nil || durable.Config.Worker.HCGReporting.Enabled == nil || !*durable.Config.Worker.HCGReporting.Enabled {
				t.Fatalf("final durable policy not true: %+v %v", durable, err)
			}
			svc.ReportingLaunchReady(readyNotice(t, st, root, id, true))
			waitMetadata(t, svc)
			v, err := svc.Get(t.Context(), id)
			if err != nil || !v.EffectiveEnabled || v.Reason == "manifest_unavailable" {
				t.Fatalf("reordered notification hid final durable policy from fresh launch: %+v %v", v, err)
			}
		})
	}
}
