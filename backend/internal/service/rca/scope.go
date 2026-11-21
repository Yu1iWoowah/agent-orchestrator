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
	path  string
	alias string
	dir   *os.Root
	info  fs.FileInfo
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
	if len(s.roots) != len(s.allowlist) {
		return nil, "scope_root_mismatch"
	}
	var roots []authorizedRoot
	var paths, aliases []string
	success := false
	defer func() {
		if !success {
			closeRoots(roots)
		}
	}()
	// Validate every slot before opening or enumerating any root. An absent
	// approved slot remains absent; another allowlisted slot cannot replace it.
	for slot, root := range s.roots {
		if ctx.Err() != nil {
			return nil, "canceled"
		}
		if !filepath.IsAbs(root) {
			return nil, "scope_root_mismatch"
		}
		actual, err := filepath.EvalSymlinks(root)
		if os.IsNotExist(err) {
			if filepath.Clean(root) != filepath.Clean(s.allowlist[slot]) {
				return nil, "scope_root_mismatch"
			}
			continue
		}
		if err != nil {
			return nil, "source_metadata_failed"
		}
		if filepath.Clean(actual) != filepath.Clean(s.allowlist[slot]) {
			return nil, "scope_root_mismatch"
		}
		if !containsPath(paths, actual) {
			paths = append(paths, actual)
			aliases = append(aliases, root)
		}
	}
	for i, path := range paths {
		pinned, reason := pinRoot(path)
		if reason != "" {
			return nil, reason
		}
		pinned.alias = aliases[i]
		roots = append(roots, pinned)
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
			found, reason := filenameCandidates(ctx, root, native)
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

// relativePath maps metadata names to their already-pinned authority. It does
// not consult the current filesystem or redirect into another approved root.
func (r authorizedRoot) relativePath(path string) (string, bool) {
	if !filepath.IsAbs(path) {
		return "", false
	}
	path = filepath.ToSlash(path)
	for _, name := range []string{r.path, r.alias} {
		if name == "" {
			continue
		}
		name = strings.TrimRight(filepath.ToSlash(name), "/")
		if path == name || (os.PathSeparator == '\\' && strings.EqualFold(path, name)) {
			return ".", true
		}
		prefix := name + "/"
		if len(path) < len(prefix) {
			continue
		}
		matches := path[:len(prefix)] == prefix
		if os.PathSeparator == '\\' {
			matches = strings.EqualFold(path[:len(prefix)], prefix)
		}
		if matches {
			// Preserve pending dot components; Rel and Clean change the physical target.
			return filepath.FromSlash(path[len(prefix):]), true
		}
	}
	return "", false
}

func registeredCandidate(ctx context.Context, roots []authorizedRoot, path string) (authorizedSource, string) {
	if !filepath.IsAbs(path) || filepath.Ext(path) != ".jsonl" || !strings.HasPrefix(filepath.Base(path), "rollout-") {
		return authorizedSource{}, "scope_root_mismatch"
	}
	for _, root := range roots {
		if relative, ok := root.relativePath(path); ok {
			return metadataCandidate(ctx, root, relative)
		}
	}
	return authorizedSource{}, "scope_root_mismatch"
}

func metadataCandidate(ctx context.Context, root authorizedRoot, relative string) (authorizedSource, string) {
	if filepath.IsAbs(relative) || filepath.VolumeName(relative) != "" {
		return authorizedSource{}, "scope_root_mismatch"
	}
	// Only resolved directories enter the stack. Expand pending symlinks
	// before interpreting a following dot-dot, without leaving this authority.
	pending := strings.Split(filepath.ToSlash(relative), "/")
	var resolved []string
	links := 0
	for len(pending) > 0 {
		if ctx.Err() != nil {
			return authorizedSource{}, "canceled"
		}
		part := pending[0]
		pending = pending[1:]
		switch part {
		case "", ".":
			continue
		case "..":
			if len(resolved) == 0 {
				return authorizedSource{}, "scope_root_mismatch"
			}
			resolved = resolved[:len(resolved)-1]
			continue
		}
		name := strings.Join(append(resolved, part), string(filepath.Separator))
		info, err := root.dir.Lstat(name)
		if err != nil {
			if os.IsNotExist(err) {
				return authorizedSource{}, "source_metadata_failed"
			}
			return authorizedSource{}, "scope_root_mismatch"
		}
		if info.Mode()&os.ModeSymlink != 0 {
			links++
			if links >= 40 {
				return authorizedSource{}, "scope_root_mismatch"
			}
			target, err := root.dir.Readlink(name)
			if err != nil {
				return authorizedSource{}, "source_metadata_failed"
			}
			if filepath.IsAbs(target) {
				var ok bool
				target, ok = root.relativePath(target)
				if !ok {
					return authorizedSource{}, "scope_root_mismatch"
				}
				resolved = nil
			} else if filepath.VolumeName(target) != "" {
				return authorizedSource{}, "scope_root_mismatch"
			}
			pending = append(strings.Split(filepath.ToSlash(target), "/"), pending...)
			continue
		}
		resolved = append(resolved, part)
		if len(pending) > 0 {
			if !info.IsDir() {
				return authorizedSource{}, "source_metadata_failed"
			}
			continue
		}
		if !info.Mode().IsRegular() {
			return authorizedSource{}, "source_metadata_failed"
		}
		if filepath.Ext(name) != ".jsonl" || !strings.HasPrefix(filepath.Base(name), "rollout-") {
			return authorizedSource{}, "scope_root_mismatch"
		}
		return authorizedSource{root: root.path, relative: name, info: info, rootInfo: root.info, eof: info.Size()}, ""
	}
	return authorizedSource{}, "source_metadata_failed"
}

var filenameUUID = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

func filenameCandidates(ctx context.Context, root authorizedRoot, native string) ([]authorizedSource, string) {
	// The lexical root name must still identify the pinned directory. This is
	// an identity check only; it never supplies a replacement candidate/root.
	current, err := os.Stat(root.path)
	if err != nil || !os.SameFile(root.info, current) {
		return nil, "scope_root_mismatch"
	}
	r := root.dir
	var out []authorizedSource
	reason := ""
	err = fs.WalkDir(r.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
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
		candidate, why := metadataCandidate(ctx, root, filepath.FromSlash(path))
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
