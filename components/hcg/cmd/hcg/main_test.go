package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/components/hcg/internal/usage"
)

func TestUsageCLIReadsFourSourceModes(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/usage/events.json")
	if err != nil {
		t.Fatal(err)
	}
	var pages map[string]json.RawMessage
	if err := json.Unmarshal(raw, &pages); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/usage/sessions/"), "/events")
		page, ok := pages[session]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(page)
	}))
	defer server.Close()
	address, _ := url.Parse(server.URL)
	runfile := filepath.Join(t.TempDir(), "running.json")
	if err := os.WriteFile(runfile, []byte(`{"port":`+address.Port()+`}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AO_RUN_FILE", runfile)
	t.Setenv("AO_DATA_DIR", t.TempDir())
	t.Setenv("AO_SESSION_ID", "")
	for session := range pages {
		for _, command := range []string{"sync", "events"} {
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), []string{"usage", command, "--session", session, "--json"}, &stdout, &stderr)
			var got usage.Snapshot
			if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
				t.Fatalf("output=%s err=%v", stdout.String(), err)
			}
			if code != 0 || got.ScanStatus != "complete" || got.Source != "daemon_ledger" || len(got.Events) != 2 || got.Events[1].InputTokens != nil || got.Bindings[0].Sources[1].AnomalyCount != 1 || got.Mode == "" {
				t.Fatalf("code=%d got=%+v stderr=%s", code, got, stderr.String())
			}
		}
	}
	for _, args := range [][]string{{}, {"usage", "current"}, {"usage", "events"}, {"usage", "events", "--bad"}} {
		var out bytes.Buffer
		if code := run(context.Background(), args, &out, &out); code != 2 {
			t.Fatalf("args=%v code=%d", args, code)
		}
	}
	t.Setenv("AO_SESSION_ID", "codex-chat")
	var out bytes.Buffer
	if code := run(context.Background(), []string{"usage", "events"}, &out, &out); code != 0 {
		t.Fatalf("env default code=%d output=%s", code, out.String())
	}
	t.Setenv("AO_RUN_FILE", filepath.Join(t.TempDir(), "absent"))
	out.Reset()
	if code := run(context.Background(), []string{"usage", "events"}, &out, &out); code != 1 {
		t.Fatalf("runtime code=%d", code)
	}
}
