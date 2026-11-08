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
	"sync/atomic"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/components/hcg/internal/quota"
	"github.com/aoagents/agent-orchestrator/components/hcg/internal/usage"
)

type cancelOutput struct {
	bytes.Buffer
	cancel context.CancelFunc
}

func (w *cancelOutput) Write(p []byte) (int, error) {
	n, err := w.Buffer.Write(p)
	w.cancel()
	return n, err
}
func TestQuotaCLIUsesOnlySyntheticAccountCommand(t *testing.T) {
	var scans atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scans.Add(1)
		if r.Method != "GET" || !strings.HasPrefix(r.URL.Path, "/api/v1/usage/sessions/") {
			t.Errorf("unexpected C1 request %s", r.URL)
		}
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/usage/sessions/"), "/events")
		if err := json.NewEncoder(w).Encode(usage.Page{SessionID: id, Events: []usage.Event{}, Bindings: []usage.Binding{}}); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	root := t.TempDir()
	runFile := filepath.Join(root, "run.json")
	address, _ := url.Parse(server.URL)
	if err := os.WriteFile(runFile, []byte(`{"port":`+address.Port()+`}`), 0600); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	fake := `#!/bin/sh
printf '%s\n' "$@" > "$AO_DATA_DIR/argv"
printf '%s' '{"accounts":[{"id":"a","provider":"claude","email":"private@example.com","limits":{"fetchedAt":"2026-11-09T00:00:00Z","windows":[{"id":"w","usedPercent":10,"resetsAt":"2026-11-09T05:00:00Z"}]}}]}'
`
	if err := os.WriteFile(filepath.Join(bin, "ao"), []byte(fake), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("AO_DATA_DIR", root)
	t.Setenv("AO_RUN_FILE", runFile)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := cancelOutput{cancel: cancel}
	var errors bytes.Buffer
	code := run(ctx, []string{"usage", "observe", "--sessions", "s1,s2", "--interval", "1h", "--json"}, &out, &errors)
	if code != 0 || scans.Load() != 2 {
		t.Fatalf("code=%d scans=%d err=%s output=%s", code, scans.Load(), errors.String(), out.String())
	}
	args, err := os.ReadFile(filepath.Join(root, "argv"))
	if err != nil {
		t.Fatal(err)
	}
	if string(args) != "account\nls\n--json\n" {
		t.Fatalf("unexpected account command: %q", args)
	}
	if strings.Contains(out.String(), "private@example.com") {
		t.Fatal("raw identity escaped projection")
	}
	store := quota.Store{Root: root}
	rows, err := store.Read(context.Background())
	if err != nil || len(rows) != 1 || rows[0].DataRoot != root {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	// Replace only synthetic quota history; the real reconcile caller still scans C1.
	if err := os.Remove(filepath.Join(root, "hcg", "quota", "samples.jsonl")); err != nil {
		t.Fatal(err)
	}
	first := quota.Project([]byte(`{"accounts":[{"id":"a","provider":"claude","limits":{"fetchedAt":"2026-11-09T00:00:00Z","windows":[{"id":"w","usedPercent":10,"resetsAt":"2026-11-09T05:00:00Z"}]}}]}`), nil, root, time.Date(2026, 11, 9, 0, 1, 0, 0, time.UTC))
	second := first
	second.SampledAt = first.SampledAt.Add(10 * time.Minute)
	second.Accounts = append([]quota.Account(nil), first.Accounts...)
	fetched := first.Accounts[0].FetchedAt.Add(10 * time.Minute)
	second.Accounts[0].FetchedAt = &fetched
	second.Accounts[0].Windows = append([]quota.Window(nil), first.Accounts[0].Windows...)
	v := 15.0
	second.Accounts[0].Windows[0].UsedPercent = &v
	for _, sample := range []quota.Sample{first, second} {
		if err := store.Append(context.Background(), sample); err != nil {
			t.Fatal(err)
		}
	}
	var report quota.Reconciliation
	var result bytes.Buffer
	errors.Reset()
	code = run(context.Background(), []string{"usage", "reconcile", "--sessions", "s1,s2", "--from", "2026-11-09T00:00:00Z", "--to", "2026-11-09T01:00:00Z", "--max-age", "2m", "--max-gap", "20m"}, &result, &errors)
	if err := json.Unmarshal(result.Bytes(), &report); err != nil {
		t.Fatalf("code=%d %s err=%v stderr=%s", code, result.String(), err, errors.String())
	}
	if code != 0 || scans.Load() != 4 || len(report.Segments) != 2 || report.Segments[1].Status != "unaccounted_usage_candidate" {
		t.Fatalf("code=%d report=%+v stderr=%s", code, report, errors.String())
	}
	for _, args := range [][]string{{"usage", "observe"}, {"usage", "observe", "--sessions", "s1", "--interval", "0s"}, {"usage", "observe", "--sessions", "s1,s1", "--interval", "1s"}, {"usage", "reconcile", "--sessions", "s1"}} {
		var out, errOut bytes.Buffer
		if code := run(context.Background(), args, &out, &errOut); code != 2 {
			t.Fatalf("args=%v code=%d err=%s", args, code, errOut.String())
		}
	}
	// A failed synthetic CLI may write private stderr; only a fixed code survives.
	if err := os.WriteFile(filepath.Join(bin, "ao"), []byte("#!/bin/sh\nprintf 'private@example.com raw secret' >&2\nexit 9\n"), 0700); err != nil {
		t.Fatal(err)
	}
	failedCtx, failedCancel := context.WithCancel(context.Background())
	defer failedCancel()
	failed := cancelOutput{cancel: failedCancel}
	errors.Reset()
	if code := run(failedCtx, []string{"usage", "observe", "--sessions", "s1", "--interval", "1h"}, &failed, &errors); code != 0 {
		t.Fatalf("failed observation exit=%d", code)
	}
	if !strings.Contains(failed.String(), "account_cli_failed") || strings.Contains(failed.String()+errors.String(), "secret") || strings.Contains(failed.String()+errors.String(), "private@example.com") {
		t.Fatalf("unsafe failure: %s %s", failed.String(), errors.String())
	}
	t.Setenv("AO_DATA_DIR", "")
	var noRoot bytes.Buffer
	if code := run(context.Background(), []string{"usage", "observe", "--sessions", "s1", "--interval", "1s"}, &noRoot, &errors); code != 1 {
		t.Fatal("accepted stable fallback")
	}
}
