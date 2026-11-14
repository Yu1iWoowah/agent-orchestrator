package hcg

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

type launchManifestEvidence struct {
	Revision    uint64
	Identity    struct{ SessionID, LaunchID, NativeID, Mode, ControllerGeneration string }
	MountInputs struct{ Project *bool }
	MountDigest string
}

func readLaunchManifest(root, session string) (launchManifestEvidence, bool) {
	paths, _ := filepath.Glob(filepath.Join(root, "hcg", "reporting", "*", "manifest.json"))
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		var m launchManifestEvidence
		if err == nil && json.Unmarshal(raw, &m) == nil && m.Identity.SessionID == session {
			return m, true
		}
	}
	return launchManifestEvidence{}, false
}
func waitLaunch(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("launch metadata did not settle")
		}
		time.Sleep(time.Millisecond)
	}
}
func launchFixture(t *testing.T) (*sqlite.Store, *Service, string, domain.SessionID) {
	t.Helper()
	st, err := sqlitetest.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	on := true
	if err := st.UpsertProject(t.Context(), domain.ProjectRecord{ID: "p", Path: t.TempDir(), Config: domain.ProjectConfig{HCGReporting: domain.HCGReportingConfig{Enabled: &on}}}); err != nil {
		t.Fatal(err)
	}
	rec := domain.SessionRecord{ID: "s", ProjectID: "p", Kind: domain.KindWorker, Harness: domain.HarnessCodex, Mode: domain.SessionModeChat, Metadata: domain.SessionMetadata{ProviderConversationID: "native"}}
	created, err := st.CreateSession(t.Context(), rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ClaimChatControllerGeneration(t.Context(), created.ID, "launch-1"); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	svc := New(t.Context(), root, st)
	t.Cleanup(svc.Close)
	return st, svc, root, created.ID
}
func readyNotice(t *testing.T, st *sqlite.Store, root string, id domain.SessionID, fresh bool) ports.HCGReportingLaunch {
	t.Helper()
	rec, _, err := st.GetSession(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return ports.HCGReportingLaunch{Origin: ports.HCGReportingMainChat, Owner: domain.SessionConversationOwner(rec.ID), SessionID: rec.ID, ProjectID: rec.ProjectID, Kind: rec.Kind, Harness: rec.Harness, Mode: rec.Mode, DataRoot: root, WorkspacePath: rec.Metadata.WorkspacePath, ArtifactDir: rec.Metadata.ArtifactDir, CommittedOwner: rec.ControllerOwner(), Fresh: fresh}
}
func TestLaunchReadyFreshLiveDuplicateAndLateOwner(t *testing.T) {
	st, svc, root, id := launchFixture(t)
	first := readyNotice(t, st, root, id, true)
	svc.ReportingLaunchReady(first)
	waitLaunch(t, func() bool {
		m, ok := readLaunchManifest(root, string(id))
		return ok && m.Identity.LaunchID == "launch-1"
	})
	original, _ := readLaunchManifest(root, string(id))
	off := false
	p, _, _ := st.GetProject(t.Context(), "p")
	p.Config.HCGReporting.Enabled = &off
	if err := st.UpsertProject(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	svc.ProjectPolicyChanged("p", p.Config.ReportingPolicySnapshot())
	if err := st.ClaimLiveChatControllerGeneration(t.Context(), id, "attachment-2"); err != nil {
		t.Fatal(err)
	}
	live := readyNotice(t, st, root, id, false)
	svc.ReportingLaunchReady(live)
	waitLaunch(t, func() bool {
		m, ok := readLaunchManifest(root, string(id))
		return ok && m.Identity.ControllerGeneration == "attachment-2"
	})
	kept, _ := readLaunchManifest(root, string(id))
	if kept.Identity.LaunchID != "launch-1" || kept.MountDigest != original.MountDigest || kept.MountInputs.Project == nil || !*kept.MountInputs.Project {
		t.Fatalf("live relabelled current defaults as history: %+v", kept)
	}
	if err := st.ClaimChatControllerGeneration(t.Context(), id, "launch-3"); err != nil {
		t.Fatal(err)
	}
	fresh := readyNotice(t, st, root, id, true)
	svc.ReportingLaunchReady(fresh)
	waitLaunch(t, func() bool {
		m, ok := readLaunchManifest(root, string(id))
		return ok && m.Identity.LaunchID == "launch-3"
	})
	before, _ := readLaunchManifest(root, string(id))
	if before.MountInputs.Project == nil || *before.MountInputs.Project {
		t.Fatal("same-native new host did not take new defaults")
	}
	svc.ReportingLaunchReady(first)
	svc.ReportingLaunchReady(fresh)
	waitMetadata(t, svc)
	after, _ := readLaunchManifest(root, string(id))
	if after.Identity.LaunchID != "launch-3" || after.Revision != before.Revision || after.MountDigest != before.MountDigest {
		t.Fatalf("late/duplicate notification overwrote mounting: %+v", after)
	}
}
func TestLaunchReadyReviewerSideAndUnknownAreExcluded(t *testing.T) {
	st, svc, root, id := launchFixture(t)
	base := readyNotice(t, st, root, id, true)
	for _, origin := range []ports.HCGReportingOrigin{ports.HCGReportingUnknown, ports.HCGReportingReview, ports.HCGReportingSide} {
		n := base
		n.Origin = origin
		svc.ReportingLaunchReady(n)
	}
	review := base
	review.Owner = domain.ConversationOwner{Kind: domain.ConversationOwnerReview, ID: "s"}
	svc.ReportingLaunchReady(review)
	other := base
	other.DataRoot = t.TempDir()
	svc.ReportingLaunchReady(other)
	waitMetadata(t, svc)
	if _, ok := readLaunchManifest(root, string(id)); ok {
		t.Fatal("non-main owner created a manifest")
	}
	live := base
	live.Fresh = false
	svc.ReportingLaunchReady(live)
	waitMetadata(t, svc)
	if v, err := svc.Get(t.Context(), id); err != nil || v.Reason != "manifest_unavailable" {
		t.Fatalf("missing live manifest guessed defaults: %+v %v", v, err)
	}
}
func TestLaunchReadyMetadataFailureKeepsCommittedSession(t *testing.T) {
	st, svc, root, id := launchFixture(t)
	n := readyNotice(t, st, root, id, true)
	svc.ReportingLaunchReady(n)
	waitLaunch(t, func() bool { _, ok := readLaunchManifest(root, string(id)); return ok })
	paths, _ := filepath.Glob(filepath.Join(root, "hcg", "reporting", "*", "manifest.json"))
	if err := os.Remove(paths[0]); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(paths[0], 0700); err != nil {
		t.Fatal(err)
	}
	if err := st.ClaimChatControllerGeneration(t.Context(), id, "new-owner"); err != nil {
		t.Fatal(err)
	}
	svc.ReportingLaunchReady(readyNotice(t, st, root, id, true))
	waitMetadata(t, svc)
	rec, _, err := st.GetSession(t.Context(), id)
	if err != nil || rec.Metadata.AgentSettingsLaunchID != "new-owner" || rec.Metadata.ProviderConversationID != "native" {
		t.Fatalf("HCG changed successful native ownership: %+v %v", rec.Metadata, err)
	}
	if v, err := svc.Get(t.Context(), id); err != nil || v.Available {
		t.Fatalf("corrupt manifest stayed available: %+v %v", v, err)
	}
}

func waitMetadata(t *testing.T, s *Service) {
	t.Helper()
	s.metadata.mu.Lock()
	var channels []chan struct{}
	for _, item := range s.metadata.items {
		channels = append(channels, item.done)
	}
	s.metadata.mu.Unlock()
	for _, done := range channels {
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("metadata job did not finish")
		}
	}
}

// Block only the private consumer read, after CaptureLaunch has returned.
type delayedLaunchStore struct {
	Store
	entered chan struct{}
	release chan struct{}
}

func (s delayedLaunchStore) GetSession(ctx context.Context, id domain.SessionID) (domain.SessionRecord, bool, error) {
	close(s.entered)
	select {
	case <-s.release:
		return s.Store.GetSession(ctx, id)
	case <-ctx.Done():
		return domain.SessionRecord{}, false, ctx.Err()
	}
}
func TestLaunchReadyQueueDelayDoesNotGuessLaterPolicy(t *testing.T) {
	st, svc, root, id := launchFixture(t)
	n := readyNotice(t, st, root, id, true)
	entered, release := make(chan struct{}), make(chan struct{})
	svc.store = delayedLaunchStore{Store: st, entered: entered, release: release}
	svc.ReportingLaunchReady(n)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("metadata read not reached")
	}
	p, _, err := st.GetProject(t.Context(), "p")
	if err != nil {
		t.Fatal(err)
	}
	off := false
	p.Config.HCGReporting.Enabled = &off
	if err := st.UpsertProject(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	svc.ProjectPolicyChanged("p", p.Config.ReportingPolicySnapshot())
	close(release)
	waitMetadata(t, svc)
	if _, ok := readLaunchManifest(root, string(id)); ok {
		t.Fatal("delayed notice adopted later policy")
	}
	// Even after settings change back, the same failed receipt cannot recapture.
	on := true
	p.Config.HCGReporting.Enabled = &on
	if err := st.UpsertProject(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	svc.ProjectPolicyChanged("p", p.Config.ReportingPolicySnapshot())
	svc.ReportingLaunchReady(n)
	waitMetadata(t, svc)
	if _, ok := readLaunchManifest(root, string(id)); ok {
		t.Fatal("duplicate recaptured a different launch snapshot")
	}
}
