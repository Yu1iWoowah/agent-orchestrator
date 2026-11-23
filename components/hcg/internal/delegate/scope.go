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

// scopeRevision names the files as read now; content_sha256 pins their bytes.
const scopeRevision = "current"

// ApprovedScope comes from AO's session facts, never from the tool's
// model-authored arguments. Roots[0] is the session worktree; relative paths
// resolve there, and absolute paths must fall inside one of the read-only roots.
type ApprovedScope struct {
	Name, Revision string
	Roots          []string
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

func safeText(s string) bool { return textIssue(s) == "" }

// textIssue names safeText's first failing check; it never returns the match.
func textIssue(s string) string {
	if !utf8.ValidString(s) {
		return "invalid_utf8"
	}
	if suspiciousContent.MatchString(s) {
		return "sensitive_pattern"
	}
	for _, r := range s {
		if r < 32 && r != '\n' && r != '\r' && r != '\t' {
			return "control_character"
		}
	}
	return ""
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
func textHash(s string) string  { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }
func validDigest(s string) bool { b, err := hex.DecodeString(s); return err == nil && len(b) == 32 }

// prepareScope reads each requested file once and hashes what it read. limit
// bounds the total bytes so a scope cannot exceed the model context.
func prepareScope(ctx context.Context, approved ApprovedScope, question string, paths []string, limit int64) (preparedScope, error) {
	out := preparedScope{}
	if !profileToken.MatchString(approved.Name) || !profileToken.MatchString(approved.Revision) || len(approved.Roots) == 0 || len(paths) == 0 || len(paths) > 16 || len(question) == 0 || len(question) > 16<<10 || !safeText(question) {
		return out, scopeFailure("blocked", "", "scope_not_approved")
	}
	for _, root := range approved.Roots {
		if !filepath.IsAbs(root) || filepath.Clean(root) != root {
			return out, scopeFailure("blocked", "", "scope_not_approved")
		}
	}
	manifest, err := json.Marshal(approved)
	if err != nil {
		return out, err
	}
	out.ManifestDigest = textHash(string(manifest))
	seen := map[string]bool{}
	var total int64
	for i, path := range paths {
		id := fmt.Sprintf("s%04d", i)
		if err := ctx.Err(); err != nil {
			return out, err
		}
		base, rel := approved.Roots[0], path
		if filepath.IsAbs(path) {
			base = ""
			for _, root := range approved.Roots {
				if r, err := filepath.Rel(root, path); err == nil && filepath.IsLocal(r) {
					base, rel = root, r
					break
				}
			}
			if base == "" {
				return out, scopeFailure("blocked", id, "outside_scope")
			}
		}
		if !filepath.IsLocal(rel) || filepath.Clean(rel) != rel || !safeSourcePath(rel) || seen[filepath.Join(base, rel)] {
			return out, scopeFailure("blocked", id, "outside_scope")
		}
		seen[filepath.Join(base, rel)] = true
		// OpenRoot follows symlinks inside the root, so the real target must
		// pass the same sensitive-path rules as the requested path.
		real, realErr := filepath.EvalSymlinks(filepath.Join(base, rel))
		baseReal, baseErr := filepath.EvalSymlinks(base)
		if realErr != nil || baseErr != nil {
			return out, scopeFailure("blocked", id, "source_unavailable")
		}
		if target, err := filepath.Rel(baseReal, real); err != nil || !filepath.IsLocal(target) || !safeSourcePath(target) {
			return out, scopeFailure("blocked", id, "outside_scope")
		}
		root, err := os.OpenRoot(base)
		if err != nil {
			return out, scopeFailure("blocked", id, "source_unavailable")
		}
		f, err := root.OpenFile(rel, sourceOpenFlags, 0)
		root.Close()
		if err != nil {
			return out, scopeFailure("blocked", id, "source_unavailable")
		}
		info, statErr := f.Stat()
		if statErr != nil || !info.Mode().IsRegular() {
			f.Close()
			return out, scopeFailure("blocked", id, "source_not_regular")
		}
		if total+info.Size() > limit {
			f.Close()
			return out, scopeFailure("scope_too_large", id, "payload_context_limit")
		}
		body, err := readSource(ctx, f, limit-total)
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
		total += int64(len(body))
		if total > limit {
			return out, scopeFailure("scope_too_large", id, "payload_context_limit")
		}
		if !safeText(string(body)) {
			return out, scopeFailure("blocked", id, "source_sensitive")
		}
		// Worktree files keep their relative path; other roots show where they live.
		display := filepath.ToSlash(rel)
		if base != approved.Roots[0] {
			display = filepath.ToSlash(path)
		}
		text, lines := sourceView(string(body))
		out.Sources = append(out.Sources, sourceSnapshot{SourceRange: SourceRange{ID: id, Path: display, Revision: approved.Revision, SHA256: textHash(string(body)), Start: 1, End: len(lines)}, text: text, originalText: string(body), observedAt: time.Now().UTC()})
	}
	return out, nil
}

// readSource stops one byte past limit so an oversized file is detected.
func readSource(ctx context.Context, r io.Reader, limit int64) ([]byte, error) {
	var body []byte
	buf := make([]byte, 4096)
	for int64(len(body)) <= limit {
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
