package projectdocuments

import (
	"fmt"
	"net/url"
	"path"
	"slices"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	mdtext "github.com/yuin/goldmark/text"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
)

type adoptionFileEvidence struct {
	domain.ProjectDocumentEntry
	SourcePath      string `json:"sourcePath"`
	DestinationPath string `json:"destinationPath"`
	SourceHash      string `json:"sourceHash"`
	DestinationHash string `json:"destinationHash"`
}
type adoptionManifest struct {
	ProjectID          domain.ProjectID       `json:"projectId"`
	SourceSessionID    domain.SessionID       `json:"sourceSessionId"`
	SourceRoot         string                 `json:"sourceRoot"`
	SourceRootIdentity string                 `json:"sourceRootIdentity"`
	Files              []adoptionFileEvidence `json:"files"`
	LegacyPathMap      map[string]string      `json:"legacyPathMap"`
	Diagnostics        []string               `json:"diagnostics"`
	Complete           bool                   `json:"complete"`
}

func isBackup(p string) bool {
	for _, segment := range strings.Split(p, "/") {
		if strings.Contains(segment, ".bak-") {
			return true
		}
	}
	return false
}
func sharedLink(id domain.ProjectID, p string) string {
	parts := strings.Split(p, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return "ao://projects/" + url.PathEscape(string(id)) + "/shared/" + strings.Join(parts, "/")
}

// The manifest records legacy references; it never rewrites document semantics.
func auditLivingLinks(id domain.ProjectID, legacy string, data map[string][]byte) ([]string, map[string]string, error) {
	diagnostics := []string{}
	mapping := map[string]string{}
	paths := make([]string, 0, len(data))
	for p := range data {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	for _, p := range paths {
		b := data[p]
		mapping[legacy+"/"+p] = sharedLink(id, p)
		if !strings.EqualFold(path.Ext(p), ".md") {
			continue
		}
		if strings.Contains(string(b), legacy) || strings.Contains(string(b), "ao://sessions/") {
			diagnostics = append(diagnostics, p+": legacy absolute path or session URI retained; review before updating living references")
		}
		root := goldmark.New().Parser().Parse(mdtext.NewReader(b))
		err := ast.Walk(root, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
			if !entering {
				return ast.WalkContinue, nil
			}
			var dest []byte
			switch node := n.(type) {
			case *ast.Link:
				dest = node.Destination
			case *ast.Image:
				dest = node.Destination
			default:
				return ast.WalkContinue, nil
			}
			u, err := url.Parse(string(dest))
			if err != nil {
				return ast.WalkStop, apierr.Invalid("PROJECT_DOCUMENT_ADOPTION_DEPENDENCY", "Invalid document reference in "+p, nil)
			}
			if u.IsAbs() || u.Host != "" || u.Path == "" {
				return ast.WalkContinue, nil
			}
			if strings.HasPrefix(u.Path, "/") {
				diagnostics = append(diagnostics, p+": absolute file reference retained: "+u.Path)
				return ast.WalkContinue, nil
			}
			target := path.Join(path.Dir(p), u.Path)
			if err := cleanPath(target); err != nil {
				return ast.WalkStop, apierr.Invalid("PROJECT_DOCUMENT_ADOPTION_DEPENDENCY", "Relative reference leaves the shared root: "+p, nil)
			}
			if _, ok := data[target]; !ok {
				directory := false
				for candidate := range data {
					if strings.HasPrefix(candidate, target+"/") {
						directory = true
						break
					}
				}
				if !directory {
					return ast.WalkStop, apierr.Invalid("PROJECT_DOCUMENT_ADOPTION_DEPENDENCY", fmt.Sprintf("Missing relative dependency: %s -> %s", p, target), nil)
				}
			}
			return ast.WalkContinue, nil
		})
		if err != nil {
			return nil, nil, err
		}
	}
	return diagnostics, mapping, nil
}
