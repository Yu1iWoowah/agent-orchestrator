package source

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestScanAndRepair(t *testing.T) {
	input := 100
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/usage/sessions/session/events" {
			t.Errorf("path=%s", r.URL.Path)
		}
		after := r.URL.Query().Get("afterId")
		id := 1
		more := true
		if after == "1" {
			id = 2
			more = false
			if r.URL.Query().Get("throughId") != "2" {
				t.Error("missing fixed bound")
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"sessionId": "session", "throughId": 2, "nextAfterId": id, "hasMore": more, "events": []any{map[string]any{"id": id, "bindingId": 1, "usageSourceId": 1, "sourceEventKey": strconv.Itoa(id), "inputTokens": input}}, "bindings": []any{map[string]any{"id": 1, "sessionId": "session", "harness": "codex", "sources": []any{map[string]any{"id": 1, "kind": "codex_rollout"}}}}})
	}))
	defer server.Close()
	client := Client{BaseURL: server.URL, HTTP: server.Client(), DataRoot: t.TempDir()}
	first, err := client.Scan(context.Background(), "session")
	if err != nil || first.ScanStatus != "complete" || len(first.Events) != 2 {
		t.Fatalf("snapshot=%+v err=%v", first, err)
	}
	input = 200
	second, err := client.Scan(context.Background(), "session")
	if err != nil || *second.Events[0].InputTokens != 200 || len(second.Events) != 2 {
		t.Fatalf("repair=%+v err=%v", second, err)
	}
}

func TestPartialDoesNotReusePreviousValues(t *testing.T) {
	for _, failure := range []string{"http", "id", "key", "bound", "session", "cursor"} {
		t.Run(failure, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				after := r.URL.Query().Get("afterId")
				if after == "1" && failure == "http" {
					http.Error(w, "failed", 500)
					return
				}
				id := 1
				more := true
				key := "one"
				bound := 2
				session := "s"
				cursor := 1
				if after == "1" {
					id = 2
					cursor = 2
					more = false
					key = "two"
					switch failure {
					case "id":
						id = 1
					case "key":
						key = "one"
					case "bound":
						bound = 3
					case "session":
						session = "other"
					case "cursor":
						cursor = 1
					}
				}
				json.NewEncoder(w).Encode(map[string]any{"sessionId": session, "throughId": bound, "nextAfterId": cursor, "hasMore": more, "events": []any{map[string]any{"id": id, "bindingId": 1, "usageSourceId": 1, "sourceEventKey": key}}, "bindings": []any{map[string]any{"id": 1, "sessionId": "s", "sources": []any{map[string]any{"id": 1}}}}})
			}))
			defer server.Close()
			client := Client{BaseURL: server.URL, HTTP: server.Client()}
			got, err := client.Scan(context.Background(), "s")
			if err == nil || got.ScanStatus != "partial" || len(got.Events) != 1 {
				t.Fatalf("got=%+v err=%v", got, err)
			}
		})
	}
}

func TestDataRootNeverFallsBackToStable(t *testing.T) {
	t.Setenv("AO_DATA_DIR", t.TempDir())
	t.Setenv("AO_RUN_FILE", "")
	if _, err := FromEnvironment(); err == nil {
		t.Fatal("data override used stable run-file")
	}
	root := t.TempDir()
	file := filepath.Join(root, "running.json")
	if err := os.WriteFile(file, []byte(`{"port":12345}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AO_RUN_FILE", file)
	client, err := FromEnvironment()
	if err != nil || client.BaseURL != "http://127.0.0.1:12345" || client.DataRoot != os.Getenv("AO_DATA_DIR") {
		t.Fatalf("client=%+v err=%v", client, err)
	}
}
