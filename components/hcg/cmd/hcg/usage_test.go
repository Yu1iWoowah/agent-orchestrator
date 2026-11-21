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
	"time"

	"github.com/aoagents/agent-orchestrator/components/hcg/internal/usage"
)

func TestUsageQueriesConsumeC1(t *testing.T) {
	at := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.HasPrefix(r.URL.Path, "/api/v1/usage/sessions/") || !strings.HasSuffix(r.URL.Path, "/events") {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/usage/sessions/"), "/events")
		w.Header().Set("Content-Type", "application/json")
		if id == "broken" {
			w.WriteHeader(503)
			if err := json.NewEncoder(w).Encode(map[string]string{"code": "READ_FAILED", "message": "fixture error", "requestId": "req-current"}); err != nil {
				t.Error(err)
			}
			return
		}
		input, cached, uncached, output := int64(100), int64(70), int64(30), int64(5)
		if id == "s2" {
			input = 200
			uncached = 130
		}
		page := usage.Page{SessionID: id, CurrentNativeID: "root", ThroughID: 2, NextAfterID: 2, SourceAsOf: at, MetadataAsOf: at, Bindings: []usage.Binding{{ID: 1, SessionID: id, Harness: "codex", NativeRootID: "root", State: "active", Sources: []usage.Source{{ID: 1, Kind: "codex_rollout", NativeSessionID: "root", State: "active"}}}}, Events: []usage.Event{{ID: 1, BindingID: 1, UsageSourceID: 1, SourceEventKey: "one", MeasurementKind: "native_reported", UsageProjectionStatus: "complete", InputTokens: &input, CachedInputTokens: &cached, UncachedInputTokens: &uncached, OutputTokens: &output, CreatedAt: &at, ProviderUsage: map[string]any{"last_token_usage": map[string]any{"input_tokens": 99999}}}, {ID: 2, BindingID: 1, UsageSourceID: 1, SourceEventKey: "undated", MeasurementKind: "native_reported", UsageProjectionStatus: "missing"}}}
		if id == "s2" {
			page.ThroughID = 4
			page.NextAfterID = 4
			page.Events[0].ID = 3
			page.Events[1].ID = 4
		}
		if err := json.NewEncoder(w).Encode(page); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	address, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "running.json")
	if err := os.WriteFile(file, []byte(`{"port":`+address.Port()+`}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AO_RUN_FILE", file)
	t.Setenv("AO_DATA_DIR", t.TempDir())
	t.Setenv("AO_SESSION_ID", "s1")
	t.Run("current", func(t *testing.T) {
		var out, errors bytes.Buffer
		code := run(context.Background(), []string{"usage", "current", "--json"}, &out, &errors)
		var got usage.CurrentReading
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatalf("output=%s code=%d err=%v", out.String(), code, err)
		}
		if code != 0 || got.Status != "unknown" || got.Tokens != nil || got.Band != "unknown" || got.SourceEventID != nil || got.Reason != "latest_request_unverified" {
			t.Fatalf("current=%+v code=%d errors=%s", got, code, errors.String())
		}
	})
	args := []string{"usage", "baseline", "--sessions", "s2,s1", "--from", "2026-11-08T00:00:00Z", "--to", "2026-11-09T00:00:00Z", "--json"}
	t.Run("baseline", func(t *testing.T) {
		var out, errors bytes.Buffer
		code := run(context.Background(), args, &out, &errors)
		var got usage.BaselineReport
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatalf("output=%s code=%d err=%v", out.String(), code, err)
		}
		if code != 0 || got.Status != "complete" || got.Source != "daemon_ledger" || got.SessionIDs[0] != "s2" || got.From != args[5] || got.To != args[7] || got.CaptureRate != nil || got.Recorded.EventCount != 2 || *got.Recorded.InputTokens != 300 || got.Undated.EventCount != 2 || got.Undated.InputTokens != nil {
			t.Fatalf("baseline=%+v code=%d errors=%s", got, code, errors.String())
		}
	})
	t.Run("runtime failure", func(t *testing.T) {
		for _, cmd := range [][]string{{"usage", "current", "--session", "broken"}, append(append([]string(nil), args[:3]...), append([]string{"s1,broken"}, args[4:]...)...)} {
			var out, errors bytes.Buffer
			code := run(context.Background(), cmd, &out, &errors)
			if code != 1 || !strings.Contains(out.String(), "req-current") || !strings.Contains(errors.String(), "READ_FAILED") {
				t.Fatalf("args=%v code=%d output=%s errors=%s", cmd, code, out.String(), errors.String())
			}
		}
	})
	t.Run("usage errors", func(t *testing.T) {
		for _, cmd := range [][]string{{"usage", "baseline"}, {"usage", "baseline", "--sessions", "s1,s1", "--from", args[5], "--to", args[7]}, {"usage", "baseline", "--sessions", "s1", "--from", args[7], "--to", args[5]}} {
			var out, errors bytes.Buffer
			if code := run(context.Background(), cmd, &out, &errors); code != 2 {
				t.Fatalf("args=%v code=%d output=%s", cmd, code, out.String())
			}
		}
	})
}
