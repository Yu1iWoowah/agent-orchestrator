package controllers_test

import (
	"context"
	"fmt"
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

func TestQuestionsAPIWholeCreateAndReviseByteLimit(t *testing.T) {
	for _, operation := range []string{"create", "revise"} {
		for _, extra := range []int{0, 1} {
			t.Run(fmt.Sprintf("%s/limit+%d", operation, extra), func(t *testing.T) {
				server, path := questionAPIServer(t)
				create := `{"clientKey":"deploy","audience":"user","kind":"question","payload":` + questionAPIPayload + `}`
				body, endpoint, expectedStatus, expectedVersion := create, path, http.StatusCreated, 1
				if operation == "revise" {
					if _, status, _ := doRequest(t, server, "POST", path, create); status != http.StatusCreated {
						t.Fatalf("setup create: %d", status)
					}
					body = `{"expectedVersion":1,"payload":` + questionAPIPayload + `}`
					endpoint, expectedStatus, expectedVersion = path+"/ask-1/revise", http.StatusOK, 2
				}
				// JSON whitespace is part of the entire request, not just its payload value.
				body += strings.Repeat(" ", domain.MaxAttentionCreateBytes+extra-len(body))
				response, status, _ := doRequest(t, server, "POST", endpoint, body)
				if extra == 1 {
					expectedStatus = http.StatusBadRequest
					if !reportContainsAll(string(response), "QUESTION_INVALID", "fieldErrors", "requestId") {
						t.Fatalf("missing limit envelope: %d %s", status, response)
					}
				}
				if status != expectedStatus {
					t.Fatalf("%d-byte %s: status %d, want %d", len(body), operation, status, expectedStatus)
				}
				if extra == 1 && operation == "create" {
					response, status, _ = doRequest(t, server, "GET", path, "")
					if status != http.StatusOK || !strings.Contains(string(response), `"open":0`) {
						t.Fatalf("oversize create wrote a question: %d %s", status, response)
					}
				} else {
					if extra == 1 {
						expectedVersion = 1
					}
					response, status, _ = doRequest(t, server, "GET", path+"/ask-1", "")
					if status != http.StatusOK || !strings.Contains(string(response), fmt.Sprintf(`"version":%d`, expectedVersion)) {
						t.Fatalf("wrong persisted version: %d %s", status, response)
					}
				}
			})
		}
	}
}

func TestProjectAttentionAPIValidatesAudienceAndProjectScope(t *testing.T) {
	server, _ := questionAPIServer(t)
	for _, tc := range []struct {
		path   string
		status int
	}{
		{"/api/v1/projects/ask-api/attention?audience=user&limit=1", 200},
		{"/api/v1/projects/ask-api/attention?audience=orchestrator", 200},
		{"/api/v1/projects/ask-api/attention?audience=fake", 400},
		{"/api/v1/projects/ask-api/attention?limit=101", 400},
		{"/api/v1/projects/ask-api/attention?cursor=invalid", 400},
		{"/api/v1/projects/other/attention", 404},
	} {
		body, status, _ := doRequest(t, server, "GET", tc.path, "")
		if status != tc.status {
			t.Fatalf("%s: %d %s", tc.path, status, body)
		}
	}
}

func TestQuestionsAPINotesRoundTripAndValidation(t *testing.T) {
	for _, notes := range []string{`""`, `" \n"`, `"` + strings.Repeat("界", 4001) + `"`, `"  選 A，但 PR3 先不要做\n原文 🚀  "`} {
		for _, field := range []string{"prompt", "overall"} {
			t.Run(field+notes[:2], func(t *testing.T) {
				server, path := questionAPIServer(t)
				_, status, _ := doRequest(t, server, "POST", path, `{"clientKey":"deploy","audience":"user","kind":"question","payload":`+questionAPIPayload+`}`)
				if status != http.StatusCreated {
					t.Fatal(status)
				}
				answer := `{"questionId":"ask-1","expectedVersion":1,"clientAnswerId":"reply","answers":[{"questionKey":"target","selectedOptionIds":["stage"]`
				if field == "prompt" {
					answer += `,"notes":` + notes + `}]}`
				} else {
					answer += `}],"notes":` + notes + `}`
				}
				body, status, _ := doRequest(t, server, "POST", path+"/ask-1/answer", answer)
				if !strings.Contains(notes, "PR3") {
					if status != http.StatusBadRequest || !reportContainsAll(string(body), "QUESTION_INVALID", "notes", "requestId") {
						t.Fatalf("invalid %d %s", status, body)
					}
					return
				}
				if status != http.StatusOK || !strings.Contains(string(body), `"notes":`+notes) {
					t.Fatalf("answer %d %s", status, body)
				}
				for _, route := range []string{path + "/ask-1", path + "?state=history"} {
					body, status, _ = doRequest(t, server, "GET", route, "")
					if status != http.StatusOK || !strings.Contains(string(body), `"notes":`+notes) {
						t.Fatalf("read %d %s", status, body)
					}
				}
			})
		}
	}
}
