//go:build linux

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	rh "github.com/aoagents/agent-orchestrator/backend/internal/test/reusablehandoff"
)

func project(t *checks, bin string, input rh.Input, event ports.ChatEvent) []byte {
	t.Helper()
	var parts map[string]json.RawMessage
	if err := json.Unmarshal(input.Snapshot, &parts); err != nil {
		t.Fatal(err)
	}
	var session controllers.SessionView
	if err := json.Unmarshal(parts["session"], &session); err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(parts["transcript"], &fields); err != nil {
		t.Fatal(err)
	}
	var conversation controllers.ConversationSnapshotResponse
	if err := json.Unmarshal(parts["transcript"], &conversation); err != nil {
		t.Fatal(err)
	}
	var existing []json.RawMessage
	if err := json.Unmarshal(fields["entries"], &existing); err != nil {
		t.Fatal(err)
	}
	conversation.Messages = nil
	conversation.Activities = nil
	for _, raw := range existing {
		var identity map[string]json.RawMessage
		_ = json.Unmarshal(raw, &identity)
		var id, kind string
		_ = json.Unmarshal(identity["id"], &id)
		_ = json.Unmarshal(identity["kind"], &kind)
		if id == input.ID {
			continue
		}
		if kind == "message" {
			var message controllers.ConversationMessageResponse
			_ = json.Unmarshal(raw, &message)
			conversation.Messages = append(conversation.Messages, message)
		} else {
			var activity controllers.ConversationActivityResponse
			_ = json.Unmarshal(raw, &activity)
			conversation.Activities = append(conversation.Activities, activity)
		}
	}
	var detail map[string]any
	if err := json.Unmarshal(event.Detail, &detail); err != nil {
		t.Fatal(err)
	}
	// Only identity/sequence belong to the fixture daemon. Detail/status are the untouched producer result.
	conversation.Activities = append(conversation.Activities, controllers.ConversationActivityResponse{
		Kind: "activity", ID: input.ID, TurnID: input.Turn, Sequence: input.Sequence, ActivityKind: string(event.ActivityKind),
		Status: string(event.ActivityStatus), Summary: event.Summary, Detail: detail, CreatedAt: time.Unix(0, 0).UTC().Format(time.RFC3339),
	})
	conversation.LatestSequence = max(conversation.LatestSequence, input.Sequence)
	conversation.SessionID = input.Actor
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/conversation") {
			_ = json.NewEncoder(w).Encode(conversation)
		} else if strings.HasSuffix(r.URL.Path, "/prs") {
			_ = json.NewEncoder(w).Encode(controllers.ListSessionPRsResponse{})
		} else {
			_ = json.NewEncoder(w).Encode(controllers.SessionResponse{Session: session})
		}
	}))
	defer server.Close()
	dir, err := os.MkdirTemp(os.Getenv("TMPDIR"), "rh-proj-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
	}()
	env := cliEnvironment(t, dir, server)
	out, stderr, err := invoke(bin, env, "session", "get", input.Actor, "--transcript", "500", "--cap", "0", "--json")
	if err != nil {
		t.Fatalf("CLI transcript: %v %s", err, stderr)
	}
	return []byte(out)
}
