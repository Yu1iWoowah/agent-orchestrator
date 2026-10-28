package documentread

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/sessionartifacts"
)

// A scan is cancellable and bounded independently of the legacy session listing.
// Hitting the bound is reported as incomplete, never as a complete empty catalog.
const maxVisited = 100000
const maxDepth = 64

type catalogSource struct {
	root, kind, sessionID string
	archived              bool
	target                func(string) Target
}
type pageCursor struct {
	Revision, ProjectID, Source, Archive string
	Limit, Offset                        int
}

func normalizeQuery(q CatalogQuery) (CatalogQuery, error) {
	if q.Source == "" {
		q.Source = "all"
	}
	if q.Archive == "" {
		q.Archive = "all"
	}
	if q.Limit == 0 {
		q.Limit = 100
	}
	if q.Source != "all" && q.Source != "worker" && q.Source != "orchestrator" && q.Source != "shared" {
		return q, invalid()
	}
	if q.Archive != "all" && q.Archive != "current" && q.Archive != "archive" {
		return q, invalid()
	}
	if q.Limit < 1 || q.Limit > 200 || len(q.Cursor) > 1024 {
		return q, invalid()
	}
	return q, nil
}

// Catalog reads a bounded inventory and rejects cursors from another revision.
func (s *Service) Catalog(ctx context.Context, projectID string, q CatalogQuery) (Catalog, error) {
	var out Catalog
	out.Items = []CatalogItem{}
	out.Errors = []SourceError{}
	if e := s.project(ctx, projectID); e != nil {
		return out, e
	}
	var e error
	q, e = normalizeQuery(q)
	if e != nil {
		return out, e
	}
	sources := []catalogSource{}
	if q.Source != "shared" {
		rows, err := s.store.ListSessions(ctx, domain.ProjectID(projectID))
		if err != nil {
			out.Errors = append(out.Errors, SourceError{Source: "worker", Code: "DOCUMENT_SOURCE_UNAVAILABLE"}, SourceError{Source: "orchestrator", Code: "DOCUMENT_SOURCE_UNAVAILABLE"})
		}
		for _, rec := range rows {
			if rec.ProjectID != domain.ProjectID(projectID) {
				continue
			}
			kind := "worker"
			if rec.Kind == domain.KindOrchestrator {
				kind = "orchestrator"
			}
			if q.Source != "all" && q.Source != kind || q.Archive == "current" && rec.IsTerminated || q.Archive == "archive" && !rec.IsTerminated {
				continue
			}
			root, err := s.artifactRoot(rec)
			if err != nil {
				out.Errors = append(out.Errors, SourceError{Source: kind, SessionID: string(rec.ID), Code: "DOCUMENT_SOURCE_UNAVAILABLE"})
				continue
			}
			id := string(rec.ID)
			sources = append(sources, catalogSource{root: root, kind: kind, sessionID: id, archived: rec.IsTerminated, target: func(p string) Target {
				return Target{ArtifactTarget{Kind: "artifact", ProjectID: projectID, SessionID: id, Path: p}}
			}})
		}
	}
	if (q.Source == "all" || q.Source == "shared") && q.Archive != "archive" {
		root, err := s.shared.ReadRoot(ctx, domain.ProjectID(projectID))
		if err != nil {
			out.Errors = append(out.Errors, SourceError{Source: "shared", Code: "DOCUMENT_SOURCE_UNAVAILABLE"})
		} else {
			sources = append(sources, catalogSource{root: root, kind: "shared", target: func(p string) Target { return Target{SharedTarget{Kind: "shared", ProjectID: projectID, Path: p}} }})
		}
	}
	// Stable traversal order also makes an incomplete, bounded scan reproducible.
	sort.Slice(sources, func(i, j int) bool {
		return sources[i].kind+sources[i].sessionID < sources[j].kind+sources[j].sessionID
	})
	visited := 0
	for _, src := range sources {
		if e = ctx.Err(); e != nil {
			return out, e
		}
		items, code, err := s.scan(ctx, src, &visited)
		if err != nil {
			return out, err
		}
		out.Items = append(out.Items, items...)
		if code != "" {
			out.Errors = append(out.Errors, SourceError{Source: src.kind, SessionID: src.sessionID, Code: code})
		}
	}
	sort.Slice(out.Items, func(i, j int) bool {
		a, b := out.Items[i], out.Items[j]
		if a.UpdatedAt != nil && b.UpdatedAt != nil && !a.UpdatedAt.Equal(*b.UpdatedAt) {
			return a.UpdatedAt.After(*b.UpdatedAt)
		}
		if (a.UpdatedAt == nil) != (b.UpdatedAt == nil) {
			return a.UpdatedAt != nil
		}
		return canonical(a.Target) < canonical(b.Target)
	})
	out.Incomplete = len(out.Errors) > 0
	inventory, err := json.Marshal(out)
	if err != nil {
		return out, err
	}
	sum := sha256.Sum256(inventory)
	out.Revision = hex.EncodeToString(sum[:])
	offset := 0
	if q.Cursor != "" {
		b, err := base64.RawURLEncoding.DecodeString(q.Cursor)
		if err != nil {
			return Catalog{}, invalid()
		}
		var c pageCursor
		if err = json.Unmarshal(b, &c); err != nil {
			return Catalog{}, invalid()
		}
		if c.ProjectID != projectID || c.Source != q.Source || c.Archive != q.Archive || c.Limit != q.Limit || c.Revision != out.Revision || c.Offset <= 0 || c.Offset >= len(out.Items) {
			return Catalog{}, apierr.Conflict("DOCUMENT_CATALOG_CHANGED", "The catalog changed or the cursor is invalid; refresh the first page", nil)
		}
		offset = c.Offset
	}
	end := offset + q.Limit
	if end < len(out.Items) {
		b, err := json.Marshal(pageCursor{Revision: out.Revision, ProjectID: projectID, Source: q.Source, Archive: q.Archive, Limit: q.Limit, Offset: end})
		if err != nil {
			return Catalog{}, err
		}
		out.NextCursor = base64.RawURLEncoding.EncodeToString(b)
	} else {
		end = len(out.Items)
	}
	out.Items = out.Items[offset:end]
	return out, nil
}
func (s *Service) scan(ctx context.Context, src catalogSource, visited *int) ([]CatalogItem, string, error) {
	items := []CatalogItem{}
	root, e := s.openRoot(src.root)
	if errors.Is(e, fs.ErrNotExist) {
		return items, "", nil
	}
	if e != nil {
		return items, "DOCUMENT_SOURCE_UNAVAILABLE", nil
	}
	defer root.Close()
	code := ""
	var walk func(*os.Root, string, int) error
	walk = func(dir *os.Root, prefix string, depth int) error {
		if e := ctx.Err(); e != nil {
			return e
		}
		if depth > maxDepth {
			code = "DOCUMENT_SCAN_LIMIT"
			return nil
		}
		f, e := dir.Open(".")
		if e != nil {
			code = "DOCUMENT_SOURCE_UNAVAILABLE"
			return nil
		}
		defer f.Close()
		for {
			entries, e := f.ReadDir(128)
			if e != nil && e != io.EOF {
				code = "DOCUMENT_SOURCE_UNAVAILABLE"
				return nil
			}
			for _, entry := range entries {
				if e := ctx.Err(); e != nil {
					return e
				}
				*visited++
				if *visited > maxVisited {
					code = "DOCUMENT_SCAN_LIMIT"
					return nil
				}
				name := entry.Name()
				if name == ".versions" || strings.HasPrefix(name, ".ao-") || src.kind != "shared" && sessionartifacts.Ignored(name) {
					continue
				}
				rel := path.Join(prefix, name)
				if !validPath(rel) || entry.Type()&os.ModeSymlink != 0 {
					code = "DOCUMENT_UNSAFE_ENTRY"
					continue
				}
				if entry.IsDir() {
					child, e := subdir(dir, name)
					if e != nil {
						code = "DOCUMENT_SOURCE_UNAVAILABLE"
						continue
					}
					e = walk(child, rel, depth+1)
					child.Close()
					if e != nil {
						return e
					}
					if *visited > maxVisited {
						return nil
					}
					continue
				}
				info, e := s.metadata(ctx, root, src.root, rel)
				if e != nil {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					code = "DOCUMENT_UNSAFE_ENTRY"
					continue
				}
				items = append(items, CatalogItem{Target: src.target(rel), Path: rel, Name: name, Source: src.kind, Archived: src.archived, Size: info.Size(), UpdatedAt: updatedAt(info), Fingerprint: fingerprint(info)})
			}
			if e == io.EOF {
				break
			}
		}
		return nil
	}
	if e = walk(root, "", 0); e != nil {
		return nil, "", e
	}
	return items, code, nil
}
