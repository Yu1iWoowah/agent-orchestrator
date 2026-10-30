package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestOrchestratorReplaceAcceptsWithoutRunningLifecycle(t *testing.T) {
	cfg := setConfigEnv(t)
	var requests []string
	const key = "6f94c7ce-9e73-4ec1-8c27-4602ef1126b5"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/internal/") {
			return
		}
		requests = append(requests, r.Method+" "+r.URL.Path)
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("request body: %v", err)
		}
		if body["sourceSessionId"] != "source" || body["operationKey"] != key {
			t.Errorf("accept body = %#v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{"operation":{"id":"`+key+`","operationKey":"`+key+`","projectId":"demo","sourceSessionId":"source","phase":"accepted"}}`)
	}))
	t.Cleanup(srv.Close)
	writeRunFileFor(t, cfg, srv)
	out, stderr, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "orchestrator", "replace", "--project", "demo", "--from", "source", "--operation-key", key, "--json")
	if err != nil {
		t.Fatalf("replace: %v; stderr=%s", err, stderr)
	}
	var receipt struct {
		Operation struct {
			ID    string `json:"id"`
			Phase string `json:"phase"`
		} `json:"operation"`
	}
	if err := json.Unmarshal([]byte(out), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Operation.ID != key || receipt.Operation.Phase != "accepted" {
		t.Fatalf("receipt = %s", out)
	}
	if want := []string{"POST /api/v1/projects/demo/orchestrator/replacements"}; !reflect.DeepEqual(requests, want) {
		t.Fatalf("requests = %#v, want %#v", requests, want)
	}
}
