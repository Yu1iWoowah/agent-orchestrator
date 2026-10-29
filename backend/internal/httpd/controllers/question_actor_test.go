package controllers_test

import (
	"context"
	"encoding/json"
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
	"github.com/aoagents/agent-orchestrator/backend/internal/service/browser"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/questions"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

func TestReportForwardRequiresTheCurrentOrchestratorCapability(t *testing.T) {
	db := sqlitetest.MustOpen(t)
	ctx := context.Background()
	now := time.Now().UTC()
	authority := browser.NewAuthority()
	create := func(project string, kind domain.SessionKind, at time.Time) (domain.SessionRecord, string) {
		t.Helper()
		if err := db.UpsertProject(ctx, domain.ProjectRecord{ID: project, Path: project, RegisteredAt: now}); err != nil {
			t.Fatal(err)
		}
		record, err := db.CreateSession(ctx, domain.SessionRecord{ProjectID: domain.ProjectID(project), Kind: kind, Harness: domain.HarnessClaudeCode, Mode: domain.SessionModeChat, CreatedAt: at, UpdatedAt: at})
		if err != nil {
			t.Fatal(err)
		}
		token, verifier, err := authority.Issue(record.ID)
		if err != nil {
			t.Fatal(err)
		}
		record.Metadata.BrowserCapabilityVerifier = verifier
		if err := db.UpdateSession(ctx, record); err != nil {
			t.Fatal(err)
		}
		return record, token
	}
	worker, workerToken := create("actor", domain.KindWorker, now)
	previous, previousToken := create("actor", domain.KindOrchestrator, now)
	active, activeToken := create("actor", domain.KindOrchestrator, now.Add(time.Second))
	foreign, foreignToken := create("foreign", domain.KindOrchestrator, now)
	if _, err := db.CreateReport(ctx, domain.ReportRecord{ID: "report", SessionID: worker.ID, ProjectID: "actor", State: domain.ReportNeedsInput, Note: "Decision", CreatedAt: now, AvailableAt: now, DeliveryState: domain.ReportPending, RepeatCount: 1}); err != nil {
		t.Fatal(err)
	}
	svc := questions.New(questions.Deps{Store: db, Capabilities: authority})
	server := httptest.NewServer(httpd.NewRouterWithControl(config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, httpd.APIDeps{Questions: svc}, httpd.ControlDeps{}))
	defer server.Close()
	for _, tc := range []struct {
		name   string
		actor  domain.SessionID
		token  string
		status int
	}{
		{"forged active ID without proof", active.ID, "", http.StatusForbidden},
		{"worker token with active ID", active.ID, workerToken, http.StatusForbidden},
		{"wrong token", active.ID, "forged", http.StatusForbidden},
		{"worker identity", worker.ID, workerToken, http.StatusForbidden},
		{"foreign project", foreign.ID, foreignToken, http.StatusForbidden},
		{"replaced orchestrator", previous.ID, previousToken, http.StatusForbidden},
		{"active owner", active.ID, activeToken, http.StatusCreated},
		{"active retry", active.ID, activeToken, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"clientKey":"forward","audience":"user","kind":"question","sourceReportId":"report","payload":` + questionAPIPayload + `}`
			req, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/sessions/"+string(tc.actor)+"/questions", strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-AO-Browser-Capability", tc.token)
			response, err := server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			raw, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != tc.status {
				t.Fatalf("status=%d want=%d", response.StatusCode, tc.status)
			}
			if strings.Contains(string(raw), activeToken) || strings.Contains(string(raw), active.Metadata.BrowserCapabilityVerifier) {
				t.Fatal("actor proof leaked in response")
			}
			if tc.status < 300 {
				var question struct {
					OriginSessionID domain.SessionID `json:"originSessionId"`
					SourceReportID  string           `json:"sourceReportId"`
				}
				if err := json.Unmarshal(raw, &question); err != nil || question.OriginSessionID != worker.ID || question.SourceReportID != "report" {
					t.Fatalf("wrong canonical source: %+v %v", question, err)
				}
			}
		})
	}
}
