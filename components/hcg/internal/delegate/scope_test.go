package delegate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func approvedRequest(t *testing.T, c *Consumer, question string, paths ...string) Request {
	t.Helper()
	if len(paths) == 0 {
		p := filepath.Join(t.TempDir(), "source.txt")
		if err := os.WriteFile(p, []byte("synthetic reviewed source\n"), 0600); err != nil {
			t.Fatal(err)
		}
		paths = []string{p}
	}
	root := filepath.Dir(paths[0])
	c.ApprovedScope = ApprovedScope{Name: "synthetic-corpus", Revision: "r1", Root: root}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			t.Fatal(err)
		}
		c.ApprovedScope.Files = append(c.ApprovedScope.Files, ApprovedFile{Path: rel, SHA256: textHash(string(b))})
	}
	return Request{Question: question, Paths: paths}
}

func TestApprovedScopeRejectsChangedSensitiveAndEscapingSources(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "prompt.txt")
	if err := os.WriteFile(path, []byte("reviewed public fixture\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c := Consumer{}
	req := approvedRequest(t, &c, "inspect this", path)
	scope, err := prepareScope(t.Context(), c.ApprovedScope, req.Question, req.Paths)
	if err != nil || scope.Sources[0].ID != "s0000" || scope.Sources[0].Path != "prompt.txt" {
		t.Fatalf("approved scope: %+v %v", scope, err)
	}
	if err := os.WriteFile(path, []byte("changed after approval"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareScope(t.Context(), c.ApprovedScope, req.Question, req.Paths); err == nil {
		t.Fatal("accepted changed snapshot")
	}
	if err := os.WriteFile(path, []byte("HCG_CANARY_DO_NOT_SEND"), 0600); err != nil {
		t.Fatal(err)
	}
	req = approvedRequest(t, &c, "inspect this", path)
	if _, err := prepareScope(t.Context(), c.ApprovedScope, req.Question, req.Paths); err == nil {
		t.Fatal("approval bypassed canary stop")
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Skip(err)
	}
	c.ApprovedScope.Files = []ApprovedFile{{Path: "link.txt", SHA256: textHash("outside")}}
	if _, err := prepareScope(t.Context(), c.ApprovedScope, "inspect", []string{link}); err == nil {
		t.Fatal("followed escaping symlink")
	}
	for _, path := range []string{".env", ".ssh/id_rsa", "credentials.json", "safe/secret.key"} {
		if safeSourcePath(path) {
			t.Fatalf("accepted secret path %q", path)
		}
	}
	if safeText("api_key=" + "sk-" + strings.Repeat("x", 24)) {
		t.Fatal("accepted synthetic credential pattern")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := prepareScope(ctx, c.ApprovedScope, "inspect", []string{link}); err != context.Canceled {
		t.Fatalf("cancel=%v", err)
	}
}
