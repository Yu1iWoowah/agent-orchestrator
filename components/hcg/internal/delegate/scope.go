package delegate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const maxSourceBytes = 64 << 10

// ApprovedScope comes from AO's reviewed synthetic/public input manifest, never
// from the tool's model-authored arguments. Hash changes require new approval.
type ApprovedScope struct {
	Name, Revision, Root string
	Files                []ApprovedFile
}
type ApprovedFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}
type SourceRange struct {
	ID       string `json:"id"`
	Path     string `json:"path"`
	Revision string `json:"revision"`
	SHA256   string `json:"sha256"`
	Start    int    `json:"startLine"`
	End      int    `json:"endLine"`
}
type Omission struct {
	SourceID string `json:"sourceId,omitempty"`
	Reason   string `json:"reason"`
}
type ScopeError struct {
	Status    string
	Omissions []Omission
}

func (e *ScopeError) Error() string {
	if len(e.Omissions) > 0 {
		return e.Status + ": " + e.Omissions[0].Reason
	}
	return e.Status
}
func scopeFailure(status, id, reason string) error {
	return &ScopeError{Status: status, Omissions: []Omission{{SourceID: id, Reason: reason}}}
}

type sourceSnapshot struct {
	SourceRange
	text         string
	originalText string
	observedAt   time.Time
}
type preparedScope struct {
	Sources        []sourceSnapshot
	ManifestDigest string
}

// This narrow deny scanner is a final stop, not proof arbitrary data is safe.
// Named approval remains mandatory even when this scanner finds nothing.
var suspiciousContent = regexp.MustCompile(`(?i)(HCG_CANARY_DO_NOT_SEND|-----BEGIN [A-Z ]*PRIVATE KEY|\bsk-[a-z0-9_-]{16,}|\bAKIA[A-Z0-9]{16}\b|\bbearer\s+[a-z0-9._-]{12,}|\b(?:api[_ -]?key|password|secret|access[_ -]?token)\s*[:=]\s*["']?[^\s"']+)`)

func safeText(s string) bool {
	if !utf8.ValidString(s) || suspiciousContent.MatchString(s) {
		return false
	}
	for _, r := range s {
		if r < 32 && r != '\n' && r != '\r' && r != '\t' {
			return false
		}
	}
	return true
}
func safeSourcePath(path string) bool {
	if !safeText(path) {
		return false
	}
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		p := strings.ToLower(part)
		switch p {
		case ".git", ".ssh", ".aws", ".config", "secret", "secrets", ".env", ".npmrc", ".netrc", "auth.json", "credentials.json", "id_rsa", "id_ed25519":
			return false
		}
		if strings.HasPrefix(p, ".env.") || strings.HasPrefix(p, "secret.") || strings.HasPrefix(p, "secrets.") || strings.Contains(p, "credential") || strings.HasSuffix(p, ".pem") || strings.HasSuffix(p, ".key") || strings.HasSuffix(p, ".p12") {
			return false
		}
	}
	return true
}
func textHash(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }

func prepareScope(ctx context.Context, approved ApprovedScope, question string, paths []string) (preparedScope, error) {
	out := preparedScope{}
	if !profileToken.MatchString(approved.Name) || !profileToken.MatchString(approved.Revision) || !filepath.IsAbs(approved.Root) || len(paths) == 0 || len(paths) > 16 || len(question) == 0 || len(question) > 16<<10 || !safeText(question) {
		return out, scopeFailure("blocked", "", "scope_not_approved")
	}
	manifest, err := json.Marshal(approved)
	if err != nil {
		return out, err
	}
	out.ManifestDigest = textHash(string(manifest))
	allowed := map[string]string{}
	for _, file := range approved.Files {
		if filepath.IsAbs(file.Path) || filepath.Clean(file.Path) != file.Path || !filepath.IsLocal(file.Path) || !safeSourcePath(file.Path) || len(file.SHA256) != 64 || allowed[file.Path] != "" {
			return out, scopeFailure("blocked", "", "invalid_input_manifest")
		}
		if _, err := hex.DecodeString(file.SHA256); err != nil {
			return out, scopeFailure("blocked", "", "invalid_input_manifest")
		}
		allowed[file.Path] = file.SHA256
	}
	root, err := os.OpenRoot(approved.Root)
	if err != nil {
		return out, scopeFailure("blocked", "", "source_unavailable")
	}
	defer root.Close()
	seen := map[string]bool{}
	total := 0
	for i, path := range paths {
		id := fmt.Sprintf("s%04d", i)
		if err := ctx.Err(); err != nil {
			return out, err
		}
		if filepath.IsAbs(path) {
			path, err = filepath.Rel(approved.Root, path)
			if err != nil {
				return out, scopeFailure("blocked", id, "outside_scope")
			}
		}
		if !filepath.IsLocal(path) || filepath.Clean(path) != path || !safeSourcePath(path) || allowed[path] == "" || seen[path] {
			return out, scopeFailure("blocked", id, "outside_scope")
		}
		seen[path] = true
		f, err := root.OpenFile(path, sourceOpenFlags, 0)
		if err != nil {
			return out, scopeFailure("blocked", id, "source_unavailable")
		}
		info, statErr := f.Stat()
		if statErr != nil || !info.Mode().IsRegular() {
			f.Close()
			return out, scopeFailure("blocked", id, "source_not_regular")
		}
		if info.Size() > maxSourceBytes {
			f.Close()
			return out, scopeFailure("scope_too_large", id, "source_size_limit")
		}
		body, err := readSource(ctx, f)
		closeErr := f.Close()
		if err != nil {
			if ctx.Err() != nil {
				return out, ctx.Err()
			}
			return out, scopeFailure("blocked", id, "source_read_failed")
		}
		if closeErr != nil {
			return out, scopeFailure("blocked", id, "source_read_failed")
		}
		total += len(body)
		if len(body) > maxSourceBytes || total > 2*maxSourceBytes {
			return out, scopeFailure("scope_too_large", id, "source_size_limit")
		}
		if textHash(string(body)) != allowed[path] || !safeText(string(body)) {
			return out, scopeFailure("blocked", id, "source_changed_or_sensitive")
		}
		text, lines := sourceView(string(body))
		out.Sources = append(out.Sources, sourceSnapshot{SourceRange: SourceRange{ID: id, Path: filepath.ToSlash(path), Revision: approved.Revision, SHA256: allowed[path], Start: 1, End: len(lines)}, text: text, originalText: string(body), observedAt: time.Now().UTC()})
	}
	return out, nil
}

func readSource(ctx context.Context, r io.Reader) ([]byte, error) {
	var body []byte
	buf := make([]byte, 4096)
	for len(body) <= maxSourceBytes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, err := r.Read(buf)
		body = append(body, buf[:n]...)
		if err == io.EOF {
			return body, nil
		}
		if err != nil {
			return nil, err
		}
	}
	return body, nil
}

// sourceView maps each displayed logical line to original UTF-8 byte offsets.
// Terminators separate lines but are excluded from each line's content range.
type sourceLine struct{ start, end int }

func sourceView(raw string) (string, []sourceLine) {
	lines := []sourceLine{}
	start := 0
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\r' && raw[i] != '\n' {
			continue
		}
		lines = append(lines, sourceLine{start, i})
		if raw[i] == '\r' && i+1 < len(raw) && raw[i+1] == '\n' {
			i++
		}
		start = i + 1
	}
	lines = append(lines, sourceLine{start, len(raw)})
	var view strings.Builder
	for i, line := range lines {
		if i > 0 {
			view.WriteByte('\n')
		}
		view.WriteString(raw[line.start:line.end])
	}
	return view.String(), lines
}
