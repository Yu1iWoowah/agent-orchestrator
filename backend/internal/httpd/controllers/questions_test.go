package controllers_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/questions"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

const questionAPIPayload = `{"schemaVersion":1,"title":"Deploy target","explanation":"We need your decision before deployment","questions":[{"key":"target","header":"Target","question":"Where?","options":[{"id":"stage","label":"Staging","description":"Internal users","recommended":true},{"id":"prod","label":"Production","description":"Public users"}]}]}`

func questionAPIServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	store := sqlitetest.MustOpen(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 29, 5, 0, 0, 0, time.UTC)
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "ask-api", Path: t.TempDir(), RegisteredAt: now}); err != nil {
		t.Fatal(err)
	}
	session, err := store.CreateSession(ctx, domain.SessionRecord{ProjectID: "ask-api", Kind: domain.KindWorker, Harness: domain.HarnessClaudeCode, Mode: domain.SessionModeChat, Activity: domain.Activity{State: domain.ActivityActive, LastActivityAt: now}, CreatedAt: now, UpdatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	svc := questions.New(questions.Deps{Store: store, Now: func() time.Time { return now }, NewID: func() string { return "ask-1" }})
	server := httptest.NewServer(httpd.NewRouterWithControl(config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, httpd.APIDeps{Questions: svc}, httpd.ControlDeps{}))
	t.Cleanup(server.Close)
	return server, "/api/v1/sessions/" + string(session.ID) + "/questions"
}

func TestQuestionsAPIAllCommandsAndConflictEnvelope(t *testing.T) {
	server, path := questionAPIServer(t)
	body, status, _ := doRequest(t, server, "POST", path, `{"clientKey":"deploy","audience":"user","kind":"question","payload":`+questionAPIPayload+`}`)
	if status != http.StatusCreated || !reportContainsAll(string(body), `"id":"ask-1"`, `"version":1`, `"state":"open"`) {
		t.Fatalf("create %d %s", status, body)
	}
	body, status, _ = doRequest(t, server, "GET", path, "")
	if status != http.StatusOK || !reportContainsAll(string(body), `"open":1`, `"unread":1`) {
		t.Fatalf("list %d %s", status, body)
	}
	_, status, _ = doRequest(t, server, "POST", path+"/ask-1/read", `{"expectedVersion":1}`)
	if status != http.StatusNoContent {
		t.Fatalf("read %d", status)
	}
	body, status, _ = doRequest(t, server, "GET", path+"/ask-1", "")
	if status != http.StatusOK || !strings.Contains(string(body), `"read":true`) {
		t.Fatalf("get %d %s", status, body)
	}
	body, status, _ = doRequest(t, server, "POST", path+"/ask-1/revise", `{"expectedVersion":1,"payload":`+strings.Replace(questionAPIPayload, "Where?", "New target?", 1)+`}`)
	if status != http.StatusOK || !reportContainsAll(string(body), `"version":2`, `"read":false`) {
		t.Fatalf("revise %d %s", status, body)
	}
	answer := `{"questionId":"ask-1","expectedVersion":1,"clientAnswerId":"answer-1","answers":[{"questionKey":"target","selectedOptionIds":["stage"]}]}`
	body, status, _ = doRequest(t, server, "POST", path+"/ask-1/answer", answer)
	if status != http.StatusConflict || !reportContainsAll(string(body), "QUESTION_CHANGED", "requestId") {
		t.Fatalf("stale answer %d %s", status, body)
	}
	answer = strings.Replace(answer, `"expectedVersion":1`, `"expectedVersion":2`, 1)
	for range 2 {
		body, status, _ = doRequest(t, server, "POST", path+"/ask-1/answer", answer)
		if status != http.StatusOK || !reportContainsAll(string(body), `"deliveryState":"queued"`, `"clientAnswerId":"answer-1"`) {
			t.Fatalf("answer retry %d %s", status, body)
		}
	}
	body, status, _ = doRequest(t, server, "POST", path+"/ask-1/cancel", `{"expectedVersion":2,"actor":"user","reason":"Changed mind"}`)
	if status != http.StatusConflict {
		t.Fatalf("cancel answered %d %s", status, body)
	}
	body, status, _ = doRequest(t, server, "GET", "/api/v1/sessions/foreign/questions/ask-1", "")
	if status != http.StatusNotFound {
		t.Fatalf("foreign read %d %s", status, body)
	}
}

func TestQuestionsAPIRejectsSpoofingUnknownFieldsAndInvalidPayload(t *testing.T) {
	for _, body := range []string{
		`{"clientKey":"deploy","audience":"user","kind":"question","projectId":"spoof","payload":` + questionAPIPayload + `}`,
		`{"clientKey":"deploy","audience":"user","kind":"question","payload":` + strings.Replace(questionAPIPayload, `"title":"Deploy target"`, `"title":"Deploy target","originSessionId":"spoof"`, 1) + `}`,
		`{"clientKey":"deploy","audience":"user","kind":"question","payload":` + strings.Replace(questionAPIPayload, `"recommended":true`, `"recommended":null`, 1) + `}`,
		`{"clientKey":"deploy","clientKey":"other","audience":"user","kind":"question","payload":` + questionAPIPayload + `}`,
		`{"clientKey":"deploy","audience":"user","kind":"question","payload":` + strings.Replace(questionAPIPayload, `"explanation":"We need your decision before deployment"`, `"explanation":""`, 1) + `}`,
	} {
		t.Run(body, func(t *testing.T) {
			server, path := questionAPIServer(t)
			response, status, _ := doRequest(t, server, "POST", path, body)
			if status != http.StatusBadRequest || !reportContainsAll(string(response), "QUESTION_INVALID", "requestId", "fieldErrors") {
				t.Fatalf("invalid %d %s", status, response)
			}
			response, status, _ = doRequest(t, server, "GET", path, "")
			if status != http.StatusOK || !strings.Contains(string(response), `"open":0`) {
				t.Fatalf("invalid wrote row %d %s", status, response)
			}
		})
	}
}
