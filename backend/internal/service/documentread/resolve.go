package documentread

import (
	"context"
	"fmt"
	"mime"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/sessionartifacts"
)

func invalid() error {
	return apierr.Invalid("DOCUMENT_PATH_INVALID", "A registered, confined document path is required", nil)
}
func unavailable() error {
	return apierr.NotFound("DOCUMENT_UNAVAILABLE", "The managed document is unavailable")
}
func validID(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, "/\\%:") && !strings.ContainsFunc(s, unicode.IsControl)
}
func validPath(s string) bool {
	if s == "" || path.Clean(s) != s || strings.HasPrefix(s, "/") || strings.Contains(s, "\\") || strings.ContainsFunc(s, unicode.IsControl) {
		return false
	}
	for _, part := range strings.Split(s, "/") {
		if part == "." || part == ".." || part == ".versions" || strings.HasPrefix(part, ".ao-") {
			return false
		}
		// A literal percent (such as 50%.md) is not an encoded path segment.
		decoded, e := url.PathUnescape(part)
		if e == nil && decoded != part {
			return false
		}
	}
	return true
}
func validFragment(s *string) bool { return s == nil || !strings.ContainsFunc(*s, unicode.IsControl) }
func within(root, full string) (string, bool) {
	r, e := filepath.Rel(root, full)
	return r, e == nil && r != "." && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator)) && !filepath.IsAbs(r)
}
func (s *Service) project(ctx context.Context, id string) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	if !validID(id) {
		return invalid()
	}
	p, ok, e := s.store.GetProject(ctx, id)
	if e != nil {
		return e
	}
	if !ok || !p.ArchivedAt.IsZero() {
		return unavailable()
	}
	return nil
}
func (s *Service) artifactRoot(rec domain.SessionRecord) (string, error) {
	if !validID(string(rec.ID)) {
		return "", invalid()
	}
	root := sessionartifacts.Dir(s.dataDir, rec.ID)
	if rec.Metadata.ArtifactDir != "" && filepath.Clean(rec.Metadata.ArtifactDir) != root {
		return "", unavailable()
	}
	return root, nil
}

// Resolve verifies a registered source or maps a local path to one.
func (s *Service) Resolve(ctx context.Context, i Input) (Resolved, error) {
	if e := ctx.Err(); e != nil {
		return Resolved{}, e
	}
	switch t := i.value.(type) {
	case ArtifactTarget:
		if e := s.project(ctx, t.ProjectID); e != nil {
			return Resolved{}, e
		}
		if !validID(t.SessionID) || !validPath(t.Path) || !validFragment(t.Fragment) {
			return Resolved{}, invalid()
		}
		rows, e := s.store.ListSessions(ctx, domain.ProjectID(t.ProjectID))
		if e != nil {
			return Resolved{}, e
		}
		for _, rec := range rows {
			if string(rec.ID) == t.SessionID && string(rec.ProjectID) == t.ProjectID {
				root, e := s.artifactRoot(rec)
				if e != nil {
					return Resolved{}, e
				}
				return s.resolve(ctx, root, t.Path, Target{t})
			}
		}
		return Resolved{}, unavailable()
	case SharedTarget:
		if e := s.project(ctx, t.ProjectID); e != nil {
			return Resolved{}, e
		}
		if !validPath(t.Path) || !validFragment(t.Fragment) {
			return Resolved{}, invalid()
		}
		root, e := s.shared.ReadRoot(ctx, domain.ProjectID(t.ProjectID))
		if e != nil {
			return Resolved{}, e
		}
		return s.resolve(ctx, root, t.Path, Target{t})
	case LocalPathTarget:
		return s.resolveLocal(ctx, t.Value)
	default:
		return Resolved{}, invalid()
	}
}
func (s *Service) resolve(ctx context.Context, root, p string, t Target) (Resolved, error) {
	r, e := s.openRoot(root)
	if e != nil {
		return Resolved{}, unavailable()
	}
	defer func() { _ = r.Close() }()
	info, e := s.metadata(ctx, r, root, p)
	if e != nil {
		return Resolved{}, e
	}
	media := mime.TypeByExtension(strings.ToLower(filepath.Ext(p)))
	if media == "" {
		media = "application/octet-stream"
	}
	return Resolved{Target: t, CanonicalURI: canonical(t), LocalPath: filepath.Join(root, filepath.FromSlash(p)), Size: info.Size(), MediaType: media, UpdatedAt: updatedAt(info), Fingerprint: fingerprint(info)}, nil
}
func (s *Service) resolveLocal(ctx context.Context, value string) (Resolved, error) {
	var fragment *string
	if strings.HasPrefix(strings.ToLower(value), "file:") {
		u, e := url.Parse(value)
		if e != nil || !strings.EqualFold(u.Scheme, "file") || u.Opaque != "" || u.User != nil || (u.Host != "" && !strings.EqualFold(u.Host, "localhost")) || u.RawQuery != "" || u.ForceQuery {
			return Resolved{}, invalid()
		}
		// Reject transport-encoded separators before u.Path loses that distinction.
		escaped := strings.ToLower(u.EscapedPath())
		if strings.Contains(escaped, "%2f") || strings.Contains(escaped, "%5c") {
			return Resolved{}, invalid()
		}
		value = u.Path
		if filepath.Separator == '\\' && len(value) >= 3 && value[0] == '/' && value[2] == ':' {
			value = filepath.FromSlash(value[1:])
		}
		if u.Fragment != "" {
			fragment = &u.Fragment
		}
	}
	if strings.HasPrefix(value, "~/") {
		home, e := os.UserHomeDir()
		if e != nil {
			return Resolved{}, e
		}
		value = home + string(filepath.Separator) + value[2:]
	}
	if !filepath.IsAbs(value) || (filepath.Separator != '\\' && strings.Contains(value, "\\")) || strings.HasPrefix(value, "\\\\") || strings.ContainsFunc(value, unicode.IsControl) {
		return Resolved{}, invalid()
	}
	// Do not clean away traversal. Managed prefixes may contain projectKey escapes;
	// Resolve validates encoding only in the relative document path after matching a root.
	for i, p := range strings.Split(filepath.ToSlash(value), "/") {
		if p == ".." || p == "." || i > 0 && p == "" {
			return Resolved{}, invalid()
		}
	}
	projects, e := s.store.ListProjects(ctx)
	if e != nil {
		return Resolved{}, e
	}
	for _, project := range projects {
		if !project.ArchivedAt.IsZero() {
			continue
		}
		if e := ctx.Err(); e != nil {
			return Resolved{}, e
		}
		sessions, e := s.store.ListSessions(ctx, domain.ProjectID(project.ID))
		if e != nil {
			return Resolved{}, e
		}
		for _, rec := range sessions {
			if rec.ProjectID != domain.ProjectID(project.ID) {
				continue
			}
			root, e := s.artifactRoot(rec)
			if e != nil {
				continue
			}
			if rel, ok := within(root, value); ok {
				return s.Resolve(ctx, Input{value: ArtifactTarget{Kind: "artifact", ProjectID: project.ID, SessionID: string(rec.ID), Path: filepath.ToSlash(rel), Fragment: fragment}})
			}
		}
		root, e := s.shared.ReadRoot(ctx, domain.ProjectID(project.ID))
		if e != nil {
			continue
		}
		if rel, ok := within(root, value); ok {
			return s.Resolve(ctx, Input{value: SharedTarget{Kind: "shared", ProjectID: project.ID, Path: filepath.ToSlash(rel), Fragment: fragment}})
		}
	}
	return Resolved{}, unavailable()
}
func canonical(t Target) string {
	var prefix, p string
	var fragment *string
	switch v := t.value.(type) {
	case ArtifactTarget:
		prefix = "ao://sessions/" + url.PathEscape(v.ProjectID) + "/" + url.PathEscape(v.SessionID) + "/artifacts/"
		p = v.Path
		fragment = v.Fragment
	case SharedTarget:
		prefix = "ao://projects/" + url.PathEscape(v.ProjectID) + "/shared/"
		p = v.Path
		fragment = v.Fragment
	}
	parts := strings.Split(p, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	uri := prefix + strings.Join(parts, "/")
	if fragment != nil {
		u := url.URL{Fragment: *fragment}
		uri += "#" + u.EscapedFragment()
	}
	return uri
}
func fingerprint(info os.FileInfo) string {
	return fmt.Sprintf("%x-%x", info.Size(), info.ModTime().UnixNano())
}
