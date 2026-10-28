package projectdocuments

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
)

func cleanPath(p string) error {
	if p == "" || p == "." || path.Clean(p) != p || strings.HasPrefix(p, "/") || p == ".." || strings.HasPrefix(p, "../") || strings.ContainsAny(p, "\\\x00") {
		return apierr.Invalid("PROJECT_DOCUMENT_PATH_INVALID", "A confined relative document path is required", nil)
	}
	for _, segment := range strings.Split(p, "/") {
		if decoded, err := url.PathUnescape(segment); strings.HasPrefix(segment, ".ao-") || err == nil && decoded != segment || strings.IndexFunc(segment, func(r rune) bool { return r < 32 || r == 127 }) >= 0 {
			return apierr.Invalid("PROJECT_DOCUMENT_PATH_INVALID", "Reserved document path", nil)
		}
	}
	return nil
}
func projectKey(id domain.ProjectID) string {
	// Encode uppercase so case-insensitive hosts cannot alias two project IDs.
	// Escape Windows device names on every platform to keep the path portable.
	reserved := false
	switch strings.ToLower(string(id)) {
	case "con", "prn", "aux", "nul", "com0", "com1", "com2", "com3", "com4", "com5", "com6", "com7", "com8", "com9", "lpt0", "lpt1", "lpt2", "lpt3", "lpt4", "lpt5", "lpt6", "lpt7", "lpt8", "lpt9":
		reserved = true
	}
	var b strings.Builder
	for i, c := range []byte(id) {
		if (!reserved || i != 0) && (c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
func (s *Service) ensureRoot(root string) error {
	base, err := os.OpenRoot(s.dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = base.Close() }()
	rel, err := filepath.Rel(s.dataDir, root)
	if err != nil {
		return err
	}
	return mkdirs(base, filepath.ToSlash(rel))
}
func mkdirs(root *os.Root, dir string) error {
	if dir == "." || dir == "" {
		return nil
	}
	// Directory names may include the encoded safe project key. Public file
	// paths are validated before reaching this internal directory creator.
	if err := (domain.OrchestratorHandoffConfig{DocumentPath: dir}).Validate(); err != nil {
		return apierr.Invalid("PROJECT_DOCUMENT_UNSAFE_PATH", "Invalid managed directory path", nil)
	}
	current := ""
	for _, segment := range strings.Split(dir, "/") {
		current = path.Join(current, segment)
		info, err := root.Lstat(current)
		if errors.Is(err, fs.ErrNotExist) {
			if err = root.Mkdir(current, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
				return err
			}
			info, err = root.Lstat(current)
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return apierr.Invalid("PROJECT_DOCUMENT_UNSAFE_PATH", "Document directory is not a regular directory", nil)
		}
	}
	return nil
}
func openFile(root *os.Root, p string) (*os.File, error) {
	if err := cleanPath(p); err != nil {
		return nil, err
	}
	current := ""
	parts := strings.Split(p, "/")
	for i, part := range parts {
		current = path.Join(current, part)
		info, err := root.Lstat(current)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 || i < len(parts)-1 && !info.IsDir() || i == len(parts)-1 && !info.Mode().IsRegular() {
			return nil, apierr.NotFound("PROJECT_DOCUMENT_NOT_FOUND", "Regular project document not found")
		}
	}
	return root.Open(p)
}
func readFile(root *os.Root, p string) ([]byte, error) {
	f, err := openFile(root, p)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxFileBytes {
		return nil, apierr.Invalid("PROJECT_DOCUMENT_TOO_LARGE", "Document exceeds the preview limit. Download the original file.", nil)
	}
	return b, nil
}
func checkExpected(r *os.Root, p, expected string) error {
	b, err := readFile(r, p)
	if errors.Is(err, fs.ErrNotExist) {
		if expected == MissingHash {
			return nil
		}
		return conflict("PROJECT_DOCUMENT_STALE", "The document no longer exists; read it again before writing")
	}
	if err != nil {
		return err
	}
	if expected != hash(b) {
		return conflict("PROJECT_DOCUMENT_STALE", "The document changed; read it again before writing")
	}
	return nil
}
func listFiles(ctx context.Context, r *os.Root) ([]domain.ProjectDocumentEntry, error) {
	files := []domain.ProjectDocumentEntry{}
	err := fs.WalkDir(r.FS(), ".", func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if e := ctx.Err(); e != nil {
			return e
		}
		if strings.HasPrefix(d.Name(), ".ao-") {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return apierr.Invalid("PROJECT_DOCUMENT_UNSAFE_PATH", "Project documents contain a symlink", nil)
		}
		if d.IsDir() {
			return nil
		}
		info, e := d.Info()
		if e != nil {
			return e
		}
		if !info.Mode().IsRegular() {
			return apierr.Invalid("PROJECT_DOCUMENT_UNSAFE_PATH", "Non-regular project document", nil)
		}
		if len(files) >= maxFiles {
			return apierr.Invalid("PROJECT_DOCUMENT_LIMIT", "Too many project documents", nil)
		}
		files = append(files, domain.ProjectDocumentEntry{Path: p, Size: info.Size()})
		return nil
	})
	return files, err
}

// openRoot rejects aliases in every managed directory component, including the
// shared root itself. os.Root then confines all subsequent file operations.
func (s *Service) openRoot(root string) (*os.Root, error) {
	base, err := os.OpenRoot(s.dataDir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = base.Close() }()
	rel, err := filepath.Rel(s.dataDir, root)
	if err != nil {
		return nil, err
	}
	rel = filepath.ToSlash(rel)
	if err := (domain.OrchestratorHandoffConfig{DocumentPath: rel}).Validate(); err != nil {
		return nil, apierr.Invalid("PROJECT_DOCUMENT_UNSAFE_PATH", "Invalid managed root", nil)
	}
	current := ""
	for _, segment := range strings.Split(rel, "/") {
		current = path.Join(current, segment)
		info, e := base.Lstat(current)
		if e != nil {
			return nil, e
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, apierr.Invalid("PROJECT_DOCUMENT_UNSAFE_PATH", "The managed root contains an alias", nil)
		}
	}
	return base.OpenRoot(rel)
}
