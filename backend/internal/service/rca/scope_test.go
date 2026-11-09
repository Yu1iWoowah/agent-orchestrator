package rca

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

type scopeFixture struct {
	sessions []domain.SessionRecord
	bindings map[domain.SessionID][]domain.UsageBindingRecord
	sources  map[int64][]domain.UsageSourceRecord
	project  domain.ProjectRecord
}

func (s *scopeFixture) GetProject(context.Context, string) (domain.ProjectRecord, bool, error) {
	return s.project, true, nil
}

func (s *scopeFixture) GetSession(_ context.Context, id domain.SessionID) (domain.SessionRecord, bool, error) {
	for _, r := range s.sessions {
		if r.ID == id {
			return r, true, nil
		}
	}
	return domain.SessionRecord{}, false, nil
}
func (s *scopeFixture) ListSessions(context.Context, domain.ProjectID) ([]domain.SessionRecord, error) {
	return s.sessions, nil
}
func (s *scopeFixture) ListUsageBindingsForSession(_ context.Context, id domain.SessionID) ([]domain.UsageBindingRecord, error) {
	return s.bindings[id], nil
}
func (s *scopeFixture) ListUsageSourcesForBinding(_ context.Context, id int64) ([]domain.UsageSourceRecord, error) {
	return s.sources[id], nil
}

func scopeSetup(t *testing.T) (*scopeFixture, *scopeResolver, string) {
	t.Helper()
	r := domain.SessionRecord{ID: "selected", ProjectID: "agent-orchestrator", Kind: domain.KindWorker, Harness: domain.HarnessCodex, Mode: domain.SessionModeChat}
	r.Metadata.ProviderConversationID = syntheticNativeID
	r.Metadata.ControllerGeneration = "controller-1"
	r.Metadata.AgentSettingsLaunchID = "launch-1"
	s := &scopeFixture{sessions: []domain.SessionRecord{r}, bindings: map[domain.SessionID][]domain.UsageBindingRecord{}, sources: map[int64][]domain.UsageSourceRecord{}}
	root := t.TempDir()
	resolver := &scopeResolver{store: s, roots: []string{root}, allowlist: []string{root}}
	path := filepath.Join(root, "rollout-2026-11-09T00-00-00-"+syntheticNativeID+".jsonl")
	if err := os.WriteFile(path, []byte(header()), 0600); err != nil {
		t.Fatal(err)
	}
	return s, resolver, path
}

func TestScopeFilenameSnapshotAndRevalidation(t *testing.T) {
	s, resolver, path := scopeSetup(t)
	sources, reason := resolver.resolve(t.Context(), "selected")
	if reason != "" || len(sources) != 1 || sources[0].association != "metadata_filename" || sources[0].sourceID != 0 {
		t.Fatalf("scope failed: %s", reason)
	}
	snap := sources[0]
	if snap.eof != int64(len(header())) {
		t.Fatal("EOF not fixed")
	}
	f, reason := snap.open(t.Context())
	if reason != "" {
		t.Fatalf("open: %s", reason)
	}
	r := readCodex(t.Context(), f, snap.eof, snap.nativeID, defaultReadLimits)
	f.Close()
	if r.SourceVersion != sourceVersion {
		t.Fatal("legal synthetic content not admitted")
	}
	if !resolver.current(t.Context(), snap) {
		t.Fatal("valid scope rejected")
	}
	s.sessions[0].Metadata.ControllerGeneration = "controller-2"
	if resolver.current(t.Context(), snap) {
		t.Fatal("old owner accepted")
	}
	s.sessions[0].Metadata.ControllerGeneration = "controller-1"
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(header()), 0600); err != nil {
		t.Fatal(err)
	}
	if resolver.current(t.Context(), snap) {
		t.Fatal("replacement accepted")
	}
}

func TestScopeRootReplacementAfterValidation(t *testing.T) {
	_, resolver, path := scopeSetup(t)
	roots, reason := resolver.resolvedRoots(t.Context())
	if reason != "" || len(roots) != 1 {
		t.Fatalf("resolve roots: %s", reason)
	}
	defer closeRoots(roots)
	// registeredCandidate has already resolved this legal filename. Replace
	// its approved root before metadataCandidate accesses the file metadata.
	actual, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, filepath.Base(path)), []byte("outside must not be read"), 0600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(path)
	moved := root + "-moved"
	if err := os.Rename(root, moved); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Remove(root)
		if err := os.Rename(moved, root); err != nil {
			t.Error(err)
		}
	})
	if err := os.Symlink(outside, root); err != nil {
		t.Skip("symlinks unavailable")
	}
	candidate, reason := metadataCandidate(t.Context(), roots[0], filepath.Base(actual))
	if reason != "" || !os.SameFile(original, candidate.info) {
		t.Fatalf("metadata escaped the verified root: %s", reason)
	}
	if f, reason := candidate.open(t.Context()); f != nil || reason != "source_identity_changed" {
		if f != nil {
			_ = f.Close()
		}
		t.Fatal("replacement root admitted for content reading")
	}
	if sources, reason := filenameCandidates(t.Context(), roots[0], syntheticNativeID, roots); reason != "scope_root_mismatch" || len(sources) != 0 {
		t.Fatal("filename fallback admitted a redirected root")
	}
}

func TestPinRootRejectsRedirectedCanonicalPath(t *testing.T) {
	for _, ancestor := range []bool{false, true} {
		t.Run(map[bool]string{false: "root", true: "ancestor"}[ancestor], func(t *testing.T) {
			parent, outside := t.TempDir(), t.TempDir()
			path := filepath.Join(parent, "redirect")
			if err := os.Symlink(outside, path); err != nil {
				t.Skip("symlinks unavailable")
			}
			if ancestor {
				if err := os.Mkdir(filepath.Join(outside, "child"), 0700); err != nil {
					t.Fatal(err)
				}
				path = filepath.Join(path, "child")
			}
			root, reason := pinRoot(path)
			if root.dir != nil {
				_ = root.dir.Close()
			}
			if reason != "scope_root_mismatch" || root.dir != nil {
				t.Fatal("redirected canonical path became trusted")
			}
		})
	}
}

func TestScopeRegisteredSourceWinsAndDoesNotFallback(t *testing.T) {
	s, resolver, path := scopeSetup(t)
	s.bindings["selected"] = []domain.UsageBindingRecord{{ID: 1, SessionID: "selected", Harness: domain.HarnessCodex, NativeRootID: syntheticNativeID}}
	s.sources[1] = []domain.UsageSourceRecord{{ID: 2, BindingID: 1, Kind: domain.UsageSourceCodexRollout, NativeSessionID: syntheticNativeID, Generation: 3, ArtifactPath: path}}
	sources, reason := resolver.resolve(t.Context(), "selected")
	if reason != "" || len(sources) != 1 || sources[0].sourceID != 2 || sources[0].generation != 3 {
		t.Fatalf("registered source failed: %s", reason)
	}
	s.sources[1][0].ArtifactPath = filepath.Join(t.TempDir(), filepath.Base(path))
	if _, reason := resolver.resolve(t.Context(), "selected"); reason != "scope_root_mismatch" {
		t.Fatal("bad primary used fallback")
	}
}

func TestScopeAmbiguityAndOwnerAdmission(t *testing.T) {
	for _, change := range []string{"second_file", "other_session", "other_binding", "wrong_project", "wrong_role", "wrong_harness", "unknown_generation", "wrong_roots", "subagent_source"} {
		t.Run(change, func(t *testing.T) {
			s, resolver, path := scopeSetup(t)
			switch change {
			case "second_file":
				if err := os.WriteFile(filepath.Join(filepath.Dir(path), "rollout-other-"+syntheticNativeID+".jsonl"), []byte("not opened"), 0600); err != nil {
					t.Fatal(err)
				}
			case "other_session":
				other := s.sessions[0]
				other.ID = "other"
				s.sessions = append(s.sessions, other)
			case "other_binding":
				other := s.sessions[0]
				other.ID = "other"
				other.Metadata.ProviderConversationID = "22222222-2222-4222-8222-222222222222"
				s.sessions = append(s.sessions, other)
				s.bindings["other"] = []domain.UsageBindingRecord{{ID: 8, SessionID: "other", Harness: domain.HarnessCodex, NativeRootID: syntheticNativeID}}
			case "wrong_project":
				s.sessions[0].ProjectID = "another"
			case "wrong_role":
				s.sessions[0].Kind = domain.KindOrchestrator
			case "wrong_harness":
				s.sessions[0].Harness = domain.HarnessClaudeCode
			case "unknown_generation":
				s.sessions[0].Metadata.AgentSettingsLaunchID = ""
			case "wrong_roots":
				resolver.roots = []string{t.TempDir()}
			case "subagent_source":
				s.bindings["selected"] = []domain.UsageBindingRecord{{ID: 1, SessionID: "selected", Harness: domain.HarnessCodex, NativeRootID: syntheticNativeID}}
				s.sources[1] = []domain.UsageSourceRecord{{ID: 2, BindingID: 1, Kind: domain.UsageSourceCodexRollout, NativeSessionID: syntheticNativeID, SubagentID: "not-main", ArtifactPath: path, Generation: 1}}
			}
			if _, reason := resolver.resolve(t.Context(), "selected"); reason == "" {
				t.Fatal("rejected scope admitted")
			}
		})
	}
}

func TestScopeAliasesAndUUIDSuffix(t *testing.T) {
	_, resolver, path := scopeSetup(t)
	alias := filepath.Join(filepath.Dir(path), "rollout-alias-"+syntheticNativeID+".jsonl")
	if err := os.Link(path, alias); err != nil {
		t.Skip("hard links unavailable")
	}
	if sources, reason := resolver.resolve(t.Context(), "selected"); reason != "" || len(sources) != 1 {
		t.Fatal("same-file alias double counted")
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+"-not-exact.jsonl"); err != nil {
		t.Fatal(err)
	}
	if _, reason := resolver.resolve(t.Context(), "selected"); reason != "source_missing" {
		t.Fatal("UUID substring admitted")
	}
}

func TestScopeRootAliasAndSymlinkEscape(t *testing.T) {
	s, resolver, path := scopeSetup(t)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(filepath.Dir(path), alias); err != nil {
		t.Skip("symlinks unavailable")
	}
	resolver.roots = []string{alias}
	s.bindings["selected"] = []domain.UsageBindingRecord{{ID: 1, SessionID: "selected", Harness: domain.HarnessCodex, NativeRootID: syntheticNativeID}}
	s.sources[1] = []domain.UsageSourceRecord{{ID: 2, BindingID: 1, Kind: domain.UsageSourceCodexRollout, NativeSessionID: syntheticNativeID, Generation: 1, ArtifactPath: filepath.Join(alias, filepath.Base(path))}}
	sources, reason := resolver.resolve(t.Context(), "selected")
	if reason != "" || len(sources) != 1 {
		t.Fatal("approved final root alias rejected")
	}
	s.sources[1][0].Generation = 2
	if resolver.current(t.Context(), sources[0]) {
		t.Fatal("changed generation accepted")
	}
	outside := filepath.Join(t.TempDir(), filepath.Base(path))
	if err := os.WriteFile(outside, []byte("must not open"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	if _, reason := resolver.resolve(t.Context(), "selected"); reason != "scope_root_mismatch" {
		t.Fatal("unapproved destination accepted")
	}
}

func TestScopeTUIGenerationAndHistoricalBinding(t *testing.T) {
	s, resolver, path := scopeSetup(t)
	chat := s.sessions[0]
	s.sessions[0].Mode = domain.SessionModeTUI
	s.sessions[0].Metadata.ProviderConversationID = ""
	s.sessions[0].Metadata.RuntimeLaunchID = "launch-1"
	s.sessions[0].Metadata.AgentSessionIDLaunchID = "launch-1"
	s.sessions[0].Metadata.AgentSessionID = syntheticNativeID
	if sources, reason := resolver.resolve(t.Context(), "selected"); reason != "" || len(sources) != 1 {
		t.Fatal("verified TUI generation rejected")
	}
	s.sessions[0].Metadata.RuntimeLaunchID = "launch-2"
	if _, reason := resolver.resolve(t.Context(), "selected"); reason != "native_generation_unknown" {
		t.Fatal("stale TUI proof admitted")
	}
	s.sessions[0] = chat
	s.sessions[0].Metadata.ProviderConversationID = "22222222-2222-4222-8222-222222222222"
	s.bindings["selected"] = []domain.UsageBindingRecord{{ID: 1, SessionID: "selected", Harness: domain.HarnessCodex, NativeRootID: syntheticNativeID}}
	s.sources[1] = []domain.UsageSourceRecord{{ID: 2, BindingID: 1, Kind: domain.UsageSourceCodexRollout, NativeSessionID: syntheticNativeID, Generation: 3, ArtifactPath: path}}
	sources, reason := resolver.resolve(t.Context(), "selected")
	if reason != "" || len(sources) != 1 || sources[0].nativeID != syntheticNativeID || sources[0].bindingID != 1 || sources[0].generation != 3 {
		t.Fatal("current preferences replaced historical binding proof")
	}
	if !resolver.current(t.Context(), sources[0]) {
		t.Fatal("historical source revalidation failed")
	}
	s.sources[1][0].FileIdentity = "synthetic-wrong-file-id"
	sources, reason = resolver.resolve(t.Context(), "selected")
	if reason != "" {
		t.Fatal("metadata resolution unexpectedly read identity")
	}
	if f, reason := sources[0].open(t.Context()); reason != "source_identity_changed" || f != nil {
		if f != nil {
			_ = f.Close()
		}
		t.Fatal("registered physical identity mismatch admitted")
	}
}
