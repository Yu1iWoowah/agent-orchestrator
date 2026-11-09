package rca

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	usagesvc "github.com/aoagents/agent-orchestrator/backend/internal/service/usage"
)

type scopeStore interface {
	GetSession(context.Context, domain.SessionID) (domain.SessionRecord, bool, error)
	ListSessions(context.Context, domain.ProjectID) ([]domain.SessionRecord, error)
	ListUsageBindingsForSession(context.Context, domain.SessionID) ([]domain.UsageBindingRecord, error)
	ListUsageSourcesForBinding(context.Context, int64) ([]domain.UsageSourceRecord, error)
}

// Roots are supplied by AO's existing resolver, not HTTP. The private allowlist
// is set by daemon wiring; synthetic tests use their own private fixture roots.
type scopeResolver struct {
	store            scopeStore
	roots, allowlist []string
}

type ownerStamp struct {
	project                                                       domain.ProjectID
	kind                                                          domain.SessionKind
	harness                                                       domain.AgentHarness
	mode                                                          domain.SessionMode
	controller, launch, runtime, agentLaunch, providerID, agentID string
}

func stamp(r domain.SessionRecord) ownerStamp {
	m := r.Metadata
	return ownerStamp{r.ProjectID, r.Kind, r.Harness, r.Mode, m.ControllerGeneration, m.AgentSettingsLaunchID, m.RuntimeLaunchID, m.AgentSessionIDLaunchID, m.ProviderConversationID, m.AgentSessionID}
}

// Every field is private: a scope carries paths and native IDs only in the
// authorized daemon job. It is never a JSON/API/durable projection.
type authorizedSource struct {
	sessionID                                           domain.SessionID
	sourceID, bindingID, generation                     int64
	nativeID, root, relative, fileIdentity, association string
	owner                                               ownerStamp
	info, rootInfo                                      fs.FileInfo
	eof                                                 int64
}

func validNativeID(s string) bool {
	id, err := uuid.Parse(s)
	return err == nil && id.String() == s
}

func primaryNative(r domain.SessionRecord) (string, string) {
	if r.ProjectID != "agent-orchestrator" || r.Kind != domain.KindWorker || r.Harness != domain.HarnessCodex {
		return "", "scope_not_allowed"
	}
	m := r.Metadata
	var id string
	switch r.Mode {
	case domain.SessionModeChat:
		if m.ControllerGeneration == "" || m.AgentSettingsLaunchID == "" {
			return "", "native_generation_unknown"
		}
		id = m.ProviderConversationID
		if m.AgentSessionID != "" && m.AgentSessionID != id && (m.AgentSessionIDLaunchID == "" || m.AgentSessionIDLaunchID == m.AgentSettingsLaunchID) {
			return "", "ambiguous_source"
		}
	case domain.SessionModeTUI:
		if m.RuntimeLaunchID == "" || m.AgentSessionIDLaunchID != m.RuntimeLaunchID {
			return "", "native_generation_unknown"
		}
		id = m.AgentSessionID
		if m.ProviderConversationID != "" && m.ProviderConversationID != id && (m.AgentSettingsLaunchID == "" || m.AgentSettingsLaunchID == m.RuntimeLaunchID) {
			return "", "ambiguous_source"
		}
	default:
		return "", "native_generation_unknown"
	}
	if !validNativeID(id) {
		return "", "native_identity_unknown"
	}
	return id, ""
}

type authorizedRoot struct {
	path string
	dir  *os.Root
	info fs.FileInfo
}

func closeRoots(roots []authorizedRoot) {
	for _, root := range roots {
		_ = root.dir.Close()
	}
}

// Walk the canonical path through pinned parents. OpenRoot follows the final
// symlink, so compare each opened directory with its preceding no-follow stat
// before trusting it or accessing any of its children.
func pinRoot(path string) (authorizedRoot, string) {
	base := filepath.VolumeName(path) + string(filepath.Separator)
	r, err := os.OpenRoot(base)
	if err != nil {
		return authorizedRoot{}, "source_metadata_failed"
	}
	rel, err := filepath.Rel(base, path)
	if err != nil {
		_ = r.Close()
		return authorizedRoot{}, "scope_root_mismatch"
	}
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if part == "." {
			continue
		}
		before, err := r.Lstat(part)
		if err != nil || !before.IsDir() {
			_ = r.Close()
			return authorizedRoot{}, "scope_root_mismatch"
		}
		next, err := r.OpenRoot(part)
		_ = r.Close()
		if err != nil {
			return authorizedRoot{}, "source_metadata_failed"
		}
		after, err := next.Stat(".")
		if err != nil || !os.SameFile(before, after) {
			_ = next.Close()
			return authorizedRoot{}, "scope_root_mismatch"
		}
		r = next
	}
	info, err := r.Stat(".")
	if err != nil {
		_ = r.Close()
		return authorizedRoot{}, "source_metadata_failed"
	}
	return authorizedRoot{path: path, dir: r, info: info}, ""
}

func (s *scopeResolver) resolvedRoots(ctx context.Context) ([]authorizedRoot, string) {
	var roots []authorizedRoot
	var paths []string
	success := false
	defer func() {
		if !success {
			closeRoots(roots)
		}
	}()
	for _, root := range s.roots {
		if ctx.Err() != nil {
			return nil, "canceled"
		}
		if !filepath.IsAbs(root) {
			return nil, "scope_root_mismatch"
		}
		actual, err := filepath.EvalSymlinks(root)
		if os.IsNotExist(err) {
			if !containsPath(s.allowlist, root) {
				return nil, "scope_root_mismatch"
			}
			continue
		}
		if err != nil {
			return nil, "source_metadata_failed"
		}
		if !containsPath(s.allowlist, actual) {
			return nil, "scope_root_mismatch"
		}
		if !containsPath(paths, actual) {
			pinned, reason := pinRoot(actual)
			if reason != "" {
				return nil, reason
			}
			roots = append(roots, pinned)
			paths = append(paths, actual)
		}
	}
	success = true
	return roots, ""
}

func containsPath(paths []string, want string) bool {
	for _, p := range paths {
		if filepath.Clean(p) == filepath.Clean(want) {
			return true
		}
	}
	return false
}

func (s *scopeResolver) resolve(ctx context.Context, id domain.SessionID) ([]authorizedSource, string) {
	if ctx.Err() != nil {
		return nil, "canceled"
	}
	rec, ok, err := s.store.GetSession(ctx, id)
	if err != nil {
		return nil, "source_metadata_failed"
	}
	if !ok {
		return nil, "session_not_found"
	}
	native, reason := primaryNative(rec)
	if reason != "" {
		return nil, reason
	}
	roots, reason := s.resolvedRoots(ctx)
	if reason != "" {
		return nil, reason
	}
	defer closeRoots(roots)
	bindings, err := s.store.ListUsageBindingsForSession(ctx, id)
	if err != nil {
		return nil, "source_metadata_failed"
	}
	var out []authorizedSource
	for _, b := range bindings {
		rows, err := s.store.ListUsageSourcesForBinding(ctx, b.ID)
		if err != nil {
			return nil, "source_metadata_failed"
		}
		for _, src := range rows {
			if ctx.Err() != nil {
				return nil, "canceled"
			}
			if src.Kind != domain.UsageSourceCodexRollout {
				continue
			}
			if src.SubagentID != "" {
				if src.NativeSessionID == b.NativeRootID {
					return nil, "subagent_source"
				}
				continue
			}
			if b.SessionID != id || b.Harness != domain.HarnessCodex || src.BindingID != b.ID || src.NativeSessionID != b.NativeRootID || !validNativeID(src.NativeSessionID) {
				return nil, "source_identity_mismatch"
			}
			if src.Generation < 1 {
				return nil, "native_generation_unknown"
			}
			candidate, reason := registeredCandidate(ctx, roots, src.ArtifactPath)
			if reason != "" {
				return nil, reason
			}
			candidate.sessionID, candidate.owner, candidate.nativeID = id, stamp(rec), src.NativeSessionID
			candidate.sourceID, candidate.bindingID, candidate.generation = src.ID, b.ID, src.Generation
			candidate.fileIdentity, candidate.association = src.FileIdentity, "registered_source"
			out, reason = appendPhysical(out, candidate)
			if reason != "" {
				return nil, reason
			}
		}
	}
	if len(out) == 0 {
		if reason = s.conflict(ctx, id, native); reason != "" {
			return nil, reason
		}
		for _, root := range roots {
			found, reason := filenameCandidates(ctx, root, native, roots)
			if reason != "" {
				return nil, reason
			}
			for _, candidate := range found {
				candidate.sessionID, candidate.owner, candidate.nativeID = id, stamp(rec), native
				candidate.association = "metadata_filename"
				out, reason = appendPhysical(out, candidate)
				if reason != "" {
					return nil, reason
				}
			}
		}
		if len(out) > 1 {
			return nil, "ambiguous_source"
		}
	}
	if len(out) == 0 {
		return nil, "source_missing"
	}
	for _, candidate := range out {
		if reason := s.conflict(ctx, id, candidate.nativeID); reason != "" {
			return nil, reason
		}
	}
	return out, ""
}

func appendPhysical(out []authorizedSource, candidate authorizedSource) ([]authorizedSource, string) {
	for _, previous := range out {
		if os.SameFile(previous.info, candidate.info) {
			if previous.nativeID != candidate.nativeID || previous.generation != candidate.generation {
				return nil, "ambiguous_source"
			}
			return out, ""
		}
	}
	return append(out, candidate), ""
}

func (s *scopeResolver) conflict(ctx context.Context, selected domain.SessionID, native string) string {
	rows, err := s.store.ListSessions(ctx, "")
	if err != nil {
		return "source_metadata_failed"
	}
	for _, r := range rows {
		if ctx.Err() != nil {
			return "canceled"
		}
		if r.ID == selected {
			continue
		}
		if r.Metadata.ProviderConversationID == native || r.Metadata.AgentSessionID == native {
			return "ambiguous_source"
		}
		bindings, err := s.store.ListUsageBindingsForSession(ctx, r.ID)
		if err != nil {
			return "source_metadata_failed"
		}
		for _, b := range bindings {
			if b.NativeRootID == native {
				return "ambiguous_source"
			}
		}
	}
	return ""
}

func registeredCandidate(ctx context.Context, roots []authorizedRoot, path string) (authorizedSource, string) {
	if !filepath.IsAbs(path) || filepath.Ext(path) != ".jsonl" || !strings.HasPrefix(filepath.Base(path), "rollout-") {
		return authorizedSource{}, "scope_root_mismatch"
	}
	actual, err := filepath.EvalSymlinks(path)
	if err != nil {
		inside := false
		for _, root := range roots {
			if rel, e := filepath.Rel(root.path, path); e == nil && filepath.IsLocal(rel) {
				inside = true
				break
			}
		}
		if !inside {
			return authorizedSource{}, "scope_root_mismatch"
		}
		return authorizedSource{}, "source_metadata_failed"
	}
	path = actual
	if filepath.Ext(path) != ".jsonl" || !strings.HasPrefix(filepath.Base(path), "rollout-") {
		return authorizedSource{}, "scope_root_mismatch"
	}
	for _, root := range roots {
		rel, err := filepath.Rel(root.path, path)
		if err != nil || !filepath.IsLocal(rel) {
			continue
		}
		return metadataCandidate(ctx, root, rel)
	}
	return authorizedSource{}, "scope_root_mismatch"
}

func metadataCandidate(ctx context.Context, root authorizedRoot, relative string) (authorizedSource, string) {
	if ctx.Err() != nil {
		return authorizedSource{}, "canceled"
	}
	info, err := root.dir.Stat(relative)
	if err != nil || !info.Mode().IsRegular() {
		return authorizedSource{}, "source_metadata_failed"
	}
	return authorizedSource{root: root.path, relative: relative, info: info, rootInfo: root.info, eof: info.Size()}, ""
}

var filenameUUID = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

func filenameCandidates(ctx context.Context, root authorizedRoot, native string, roots []authorizedRoot) ([]authorizedSource, string) {
	r := root.dir
	var out []authorizedSource
	reason := ""
	err := fs.WalkDir(r.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			info, err := r.Stat(path)
			if err != nil {
				return err
			}
			if info.IsDir() {
				reason = "source_enumeration_incomplete"
				return fs.SkipAll
			}
		}
		name := entry.Name()
		if !strings.HasPrefix(name, "rollout-") || !strings.HasSuffix(name, "-"+native+".jsonl") {
			return nil
		}
		if len(filenameUUID.FindAllString(name, -1)) != 1 {
			reason = "ambiguous_source"
			return fs.SkipAll
		}
		candidate, why := registeredCandidate(ctx, roots, filepath.Join(root.path, filepath.FromSlash(path)))
		if why != "" {
			reason = why
			return fs.SkipAll
		}
		candidate.nativeID = native
		out, why = appendPhysical(out, candidate)
		if why != "" || len(out) > 1 {
			reason = "ambiguous_source"
			return fs.SkipAll
		}
		return nil
	})
	if ctx.Err() != nil {
		return nil, "canceled"
	}
	if err != nil {
		return nil, "source_enumeration_incomplete"
	}
	return out, reason
}

func (s authorizedSource) open(ctx context.Context) (*os.File, string) {
	if ctx.Err() != nil {
		return nil, "canceled"
	}
	r, err := os.OpenRoot(s.root)
	if err != nil {
		return nil, "source_identity_changed"
	}
	defer func() { _ = r.Close() }()
	rootInfo, err := r.Stat(".")
	if err != nil || !os.SameFile(s.rootInfo, rootInfo) {
		return nil, "source_identity_changed"
	}
	f, err := r.Open(s.relative)
	if err != nil {
		return nil, "source_read_failed"
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || !os.SameFile(s.info, info) || info.Size() < s.eof {
		_ = f.Close()
		return nil, "source_identity_changed"
	}
	if s.fileIdentity != "" {
		actual, err := usagesvc.SourceIdentityFromFile(f)
		if err != nil || actual != s.fileIdentity {
			_ = f.Close()
			return nil, "source_identity_changed"
		}
	}
	return f, ""
}

func (s *scopeResolver) current(ctx context.Context, source authorizedSource) bool {
	rec, ok, err := s.store.GetSession(ctx, source.sessionID)
	if err != nil || !ok || stamp(rec) != source.owner {
		return false
	}
	if _, reason := primaryNative(rec); reason != "" {
		return false
	}
	latest, reason := s.resolve(ctx, source.sessionID)
	if reason != "" {
		return false
	}
	for _, candidate := range latest {
		if candidate.sourceID == source.sourceID && candidate.bindingID == source.bindingID && candidate.generation == source.generation && candidate.nativeID == source.nativeID && candidate.fileIdentity == source.fileIdentity && os.SameFile(candidate.rootInfo, source.rootInfo) && os.SameFile(candidate.info, source.info) && candidate.eof >= source.eof {
			return true
		}
	}
	return false
}
