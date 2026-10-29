package cli

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProjectDocumentWriteJSONPreservesAuthorReceipt(t *testing.T) {
	cfg := setConfigEnv(t)
	t.Setenv("AO_SESSION_ID", "owner")
	t.Setenv("AO_BROWSER_CAPABILITY", "source-capability-secret")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/projects/demo":
			fmt.Fprint(w, `{"project":{"id":"demo"}}`)
		case strings.HasSuffix(r.URL.Path, "/lease"):
			fmt.Fprint(w, `{"lease":{"epoch":7,"token":"lease-token-secret"}}`)
		case r.Method == http.MethodPut:
			fmt.Fprint(w, `{"file":{"path":"handoff/orchestrator-handoff.md","content":"handoff","hash":"actual-hash","writeReceiptId":"actual-receipt"}}`)
		}
	}))
	defer srv.Close()
	writeRunFileFor(t, cfg, srv)
	p := filepath.Join(t.TempDir(), "draft.md")
	if err := os.WriteFile(p, []byte("handoff"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, errOut, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "project", "documents", "write", "handoff/orchestrator-handoff.md", "--project", "demo", "--file", p, "--expected-hash", "missing", "--json")
	if err != nil {
		t.Fatal(err, errOut)
	}
	if !strings.Contains(out, `"writeReceiptId": "actual-receipt"`) || !strings.Contains(out, `"hash": "actual-hash"`) {
		t.Fatal("CLI dropped author receipt/hash", out)
	}
	if strings.Contains(out, "secret") {
		t.Fatal("CLI exposed a writer secret")
	}
}
