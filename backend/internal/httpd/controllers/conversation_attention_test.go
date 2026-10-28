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

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd"
)

type fakeAttentionService struct {
	*fakeConversationService
	page     domain.ConversationAttentionPage
	err      error
	query    domain.ConversationAttentionQuery
	session  domain.SessionID
	branch   string
	activity string
	revision int64
}

func (f *fakeAttentionService) Attention(_ context.Context, session domain.SessionID, query domain.ConversationAttentionQuery) (domain.ConversationAttentionPage, error) {
	f.session, f.query = session, query
	return f.page, f.err
}

func (f *fakeAttentionService) MarkAttentionRead(_ context.Context, session domain.SessionID, branch, activity string, revision int64) error {
	f.session, f.branch, f.activity, f.revision = session, branch, activity, revision
	return f.err
}

func attentionTestServer(t *testing.T, svc *fakeAttentionService) *httptest.Server {
	t.Helper()
	svc.fakeConversationService = &fakeConversationService{}
	s := httptest.NewServer(httpd.NewRouterWithControl(config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil,
		httpd.APIDeps{Sessions: newFakeSessionService(), Conversations: svc}, httpd.ControlDeps{}))
	t.Cleanup(s.Close)
	return s
}

func TestSessionAttentionHasExplicitCapabilityFallback(t *testing.T) {
	server := planTestServer(t, &fakePlanConversationService{})
	response, err := http.Get(server.URL + "/api/v1/sessions/ao-1/attention")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusNotImplemented {
		t.Fatalf("attention status = %d, want explicit unsupported capability", response.StatusCode)
	}
}

func TestSessionAttentionWireCountsCursorAndFreshSource(t *testing.T) {
	svc := &fakeAttentionService{page: domain.ConversationAttentionPage{ConversationID: "conv", ActiveBranchID: "branch", Counts: domain.ConversationAttentionCounts{Open: 9, Unread: 4, History: 12}, HasMoreBefore: true, NextBeforeSequence: 7, Items: []domain.ConversationAttentionItem{{BranchID: "branch", Actionable: true, Activity: domain.ConversationActivity{ID: "approval", RequestID: "request", Kind: domain.ActivityKindApproval, Status: domain.ActivityStatusPending, Sequence: 7}}}}}
	server := attentionTestServer(t, svc)
	response, err := http.Get(server.URL + "/api/v1/sessions/session/attention?state=open&beforeSequence=10&limit=1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	var wire struct {
		Counts struct{ Open, Unread, History int64 }
		Items  []struct {
			Activity struct {
				RequestID string `json:"requestId"`
			}
			Actionable bool
		}
		NextBeforeSequence int64
	}
	if err := json.NewDecoder(response.Body).Decode(&wire); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || svc.session != "session" || svc.query.BeforeSequence != 10 || svc.query.Limit != 1 || wire.Counts.Open != 9 || wire.Counts.Unread != 4 || len(wire.Items) != 1 || wire.Items[0].Activity.RequestID != "request" || !wire.Items[0].Actionable || wire.NextBeforeSequence != 7 {
		t.Fatalf("wire=%+v query=%+v status=%d", wire, svc.query, response.StatusCode)
	}
	response, err = http.Get(server.URL + "/api/v1/sessions/session/attention/approval")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || svc.query.ActivityID != "approval" || svc.query.State != "all" {
		t.Fatalf("source lookup=%+v status=%d", svc.query, response.StatusCode)
	}
}

func TestSessionAttentionValidationAndSeenConflict(t *testing.T) {
	svc := &fakeAttentionService{page: domain.ConversationAttentionPage{Items: []domain.ConversationAttentionItem{}}}
	server := attentionTestServer(t, svc)
	for _, query := range []string{"state=all", "state=unknown", "limit=0", "limit=101", "beforeSequence=0", "beforeSequence=bad"} {
		r, err := http.Get(server.URL + "/api/v1/sessions/session/attention?" + query)
		if err != nil {
			t.Fatal(err)
		}
		_ = r.Body.Close()
		if r.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: status=%d", query, r.StatusCode)
		}
	}
	for _, body := range []string{`{}`, `{"expectedBranchId":"branch"}`, `{"expectedBranchId":"branch","expectedRevision":-1}`} {
		r, err := http.Post(server.URL+"/api/v1/sessions/session/attention/activity/read", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		_ = r.Body.Close()
		if r.StatusCode != http.StatusBadRequest {
			t.Fatalf("invalid read %s: status=%d", body, r.StatusCode)
		}
	}
	r, err := http.Post(server.URL+"/api/v1/sessions/session/attention/activity/read", "application/json", strings.NewReader(`{"expectedBranchId":"branch","expectedRevision":0}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Body.Close()
	if r.StatusCode != http.StatusNoContent || svc.branch != "branch" || svc.activity != "activity" || svc.revision != 0 {
		t.Fatalf("read zero revision status=%d", r.StatusCode)
	}
	svc.err = domain.ErrAttentionChanged
	r, err = http.Post(server.URL+"/api/v1/sessions/session/attention/activity/read", "application/json", strings.NewReader(`{"expectedBranchId":"branch","expectedRevision":0}`))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Body.Close() }()
	bytes, _ := io.ReadAll(r.Body)
	if r.StatusCode != http.StatusConflict || !strings.Contains(string(bytes), "ATTENTION_CHANGED") || !strings.Contains(string(bytes), "requestId") {
		t.Fatalf("conflict: status=%d body=%s", r.StatusCode, bytes)
	}
}
