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
	c.ApprovedScope = ApprovedScope{Name: "synthetic-corpus", Revision: scopeRevision, Roots: []string{filepath.Dir(paths[0])}}
	return Request{Question: question, Paths: paths}
}

func TestApprovedScopeRejectsSensitiveAndEscapingSources(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "prompt.txt")
	if err := os.WriteFile(path, []byte("reviewed public fixture\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c := Consumer{}
	req := approvedRequest(t, &c, "inspect this", path)
	scope, err := prepareScope(t.Context(), c.ApprovedScope, req.Question, req.Paths, 1<<20)
	if err != nil || scope.Sources[0].ID != "s0000" || scope.Sources[0].Path != "prompt.txt" {
		t.Fatalf("approved scope: %+v %v", scope, err)
	}
	if err := os.WriteFile(path, []byte("HCG_CANARY_DO_NOT_SEND"), 0600); err != nil {
		t.Fatal(err)
	}
	req = approvedRequest(t, &c, "inspect this", path)
	if _, err := prepareScope(t.Context(), c.ApprovedScope, req.Question, req.Paths, 1<<20); err == nil {
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
	if _, err := prepareScope(t.Context(), c.ApprovedScope, "inspect", []string{link}, 1<<20); err == nil {
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
	if _, err := prepareScope(ctx, c.ApprovedScope, "inspect", []string{link}, 1<<20); err != context.Canceled {
		t.Fatalf("cancel=%v", err)
	}
}
