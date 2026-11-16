package controllers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/requestscope"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
)

type sideHTTPService struct {
	*chatsvc.Service
	turn       *domain.SideTurn
	snapshot   *domain.SideSnapshot
	before     time.Time
	limit      int
	calls      int
	created    chatsvc.SideCreateRequest
	sent       ports.ChatUserMessage
	compactErr error
}

func (s *sideHTTPService) CompactSideChat(context.Context, domain.SessionID, string) (ports.ChatCompactionResult, error) {
	s.calls++
	return ports.ChatCompactionResult{}, s.compactErr
}

func (s *sideHTTPService) ListIndependentSideChats(context.Context, domain.SessionID) ([]domain.SideConversation, error) {
	s.calls++
	return []domain.SideConversation{{ID: "side", SessionID: "session", MainConversationID: "main", State: "ready", ProviderHostID: "private-host", ProviderForkID: "private-fork", SourceProviderID: "private-source", AppRunID: "private-launch", NativeAnchorID: "private-anchor", LaunchConfig: json.RawMessage(`{"private":"config"}`)}}, nil
}
func (s *sideHTTPService) CreateIndependentSideChat(_ context.Context, _ domain.SessionID, req chatsvc.SideCreateRequest) (domain.SideConversation, error) {
	s.calls++
	s.created = req
	return domain.SideConversation{ID: "side", State: "opening"}, nil
}
func (s *sideHTTPService) SendSideQuestion(_ context.Context, _ domain.SessionID, _ string, msg ports.ChatUserMessage) (domain.SideTurn, error) {
	s.calls++
	s.sent = msg
	if s.turn != nil {
		return *s.turn, nil
	}
	return domain.SideTurn{ID: "turn", Text: msg.Text, State: "queued", ProviderTurnID: "private-turn", Content: []domain.SideContent{{URI: "private-resource", Data: "private-image"}}, References: []domain.SideReference{{ConversationID: "main", MessageID: "source", Selection: "selected", Context: "private-context"}}}, nil
}
func (s *sideHTTPService) SideSnapshot(_ context.Context, _ domain.SessionID, _ string, before time.Time, limit int) (domain.SideSnapshot, error) {
	s.calls++
	s.before, s.limit = before, limit
	if s.snapshot != nil {
		return *s.snapshot, nil
	}
	return domain.SideSnapshot{}, chatsvc.ErrSideUnavailable
}
func sideHTTPRouter(s *sideHTTPService) http.Handler {
	return httpd.NewRouterWithControl(config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, httpd.APIDeps{Sessions: newFakeSessionService(), Conversations: s}, httpd.ControlDeps{})
}
func TestSideChatHTTPForwardsReferencesAndKeepsSummariesSafe(t *testing.T) {
	svc := &sideHTTPService{Service: chatsvc.New(chatsvc.Options{})}
	handler := sideHTTPRouter(svc)
	create := httptest.NewRecorder()
	handler.ServeHTTP(create, httptest.NewRequest(http.MethodPost, "/api/v1/sessions/session/conversation/side-chats", strings.NewReader(`{"idempotencyKey":"create","forceNew":true,"reference":{"conversationId":"main","messageId":"source","revision":3,"text":"selected"}}`)))
	if create.Code != http.StatusCreated || !svc.created.ForceNew || svc.created.Reference == nil || svc.created.Reference.MessageID != "source" {
		t.Fatalf("create: %d %s %+v", create.Code, create.Body.String(), svc.created)
	}
	send := httptest.NewRecorder()
	handler.ServeHTTP(send, httptest.NewRequest(http.MethodPost, "/api/v1/sessions/session/conversation/side-chats/side/messages", strings.NewReader(`{"text":"What is this?","clientMessageId":"receipt","references":[{"conversationId":"main","messageId":"source","revision":3,"text":"selected"}]}`)))
	if send.Code != http.StatusAccepted || svc.sent.Text != "What is this?" || len(svc.sent.Excerpts) != 1 || svc.sent.Excerpts[0].Revision != 3 {
		t.Fatalf("send: %d %s %+v", send.Code, send.Body.String(), svc.sent)
	}
	list := httptest.NewRecorder()
	handler.ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/api/v1/sessions/session/conversation/side-chats", nil))
	if list.Code != http.StatusOK {
		t.Fatal(list.Code, list.Body.String())
	}
	if bytes.Contains(list.Body.Bytes(), []byte("private")) || bytes.Contains(send.Body.Bytes(), []byte("private")) {
		t.Fatalf("private provider/context escaped: %s %s", list.Body, send.Body)
	}
	if !strings.Contains(send.Body.String(), `"messageId":"source"`) {
		t.Fatal("navigation identity omitted")
	}
}
func TestSideChatHTTPRejectsLANWithoutCallingLocalService(t *testing.T) {
	svc := &sideHTTPService{Service: chatsvc.New(chatsvc.Options{})}
	handler := sideHTTPRouter(svc)
	for _, tc := range []struct{ method, path, body string }{
		{"GET", "/api/v1/sessions/session/conversation/side-chats", ""},
		{"POST", "/api/v1/sessions/session/conversation/side-chats", `{"idempotencyKey":"create"}`},
		{"POST", "/api/v1/sessions/session/conversation/side-chats/side/messages", `{"text":"hello","clientMessageId":"receipt"}`},
		{"GET", "/api/v1/side-chats/launch/state?appRunId=launch", ""},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		req = req.WithContext(requestscope.WithLAN(req.Context()))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != http.StatusNotFound {
			t.Fatalf("%s: %d %s", tc.path, response.Code, response.Body.String())
		}
	}
	if svc.calls != 0 {
		t.Fatal("LAN reached local side service")
	}
}
func TestSideChatHTTPMapsUnavailableOwnershipToConflict(t *testing.T) {
	svc := &sideHTTPService{Service: chatsvc.New(chatsvc.Options{})}
	response := httptest.NewRecorder()
	sideHTTPRouter(svc).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/sessions/session/conversation/side-chats/wrong", nil))
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "CHAT_SIDE_UNAVAILABLE") {
		t.Fatal(response.Code, response.Body.String())
	}
}

func TestSideCompactionHTTPDistinguishesReconciliationFromAdmission(t *testing.T) {
	for _, reconciled := range []bool{true, false} {
		svc := &sideHTTPService{Service: chatsvc.New(chatsvc.Options{})}
		if reconciled {
			svc.compactErr = chatsvc.ErrSideCompactionReconciled
		}
		response := httptest.NewRecorder()
		sideHTTPRouter(svc).ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v1/sessions/session/conversation/side-chats/side/compact", nil))
		if reconciled {
			if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "CHAT_SIDE_COMPACTION_RECONCILED") || !strings.Contains(response.Body.String(), "no new compaction was started") || !strings.Contains(response.Body.String(), "submit /compact again") {
				t.Fatalf("reconciliation looked accepted: %d %s", response.Code, response.Body.String())
			}
		} else if response.Code != http.StatusAccepted {
			t.Fatalf("new compact was not accepted: %d %s", response.Code, response.Body.String())
		}
		if svc.calls != 1 {
			t.Fatalf("unexpected compact calls: %d", svc.calls)
		}
	}
}

func TestSideCompactionHTTPReportsMissingNativeHome(t *testing.T) {
	svc := &sideHTTPService{Service: chatsvc.New(chatsvc.Options{}), compactErr: chatsvc.ErrSideCompactionNativeHomeUnavailable}
	response := httptest.NewRecorder()
	sideHTTPRouter(svc).ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v1/sessions/session/conversation/side-chats/side/compact", nil))
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "CHAT_SIDE_COMPACTION_NATIVE_HOME_UNAVAILABLE") {
		t.Fatal(response.Code, response.Body.String())
	}
}

type protectedHTTPService struct {
	*sideHTTPService
	closed            string
	explicit, cleanup int
}

func (s *protectedHTTPService) ListProtectedSides(context.Context) ([]chatsvc.ProtectedSide, error) {
	return []chatsvc.ProtectedSide{{SideID: "old-side", SessionID: "missing-parent", Orphan: true, State: domain.SideCompactionUnknown}}, nil
}
func (s *protectedHTTPService) CloseProtectedSide(_ context.Context, id string) error {
	s.closed = id
	return nil
}
func (s *protectedHTTPService) ExplicitlyCloseSideCompaction(context.Context, domain.SessionID, string) error {
	s.explicit++
	return nil
}
func (s *protectedHTTPService) CloseIndependentSideChat(context.Context, domain.SessionID, string) error {
	s.cleanup++
	return nil
}
func TestProtectedSideHTTPGlobalAndExplicitClose(t *testing.T) {
	svc := &protectedHTTPService{sideHTTPService: &sideHTTPService{Service: chatsvc.New(chatsvc.Options{})}}
	handler := httpd.NewRouterWithControl(config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, httpd.APIDeps{Sessions: newFakeSessionService(), Conversations: svc}, httpd.ControlDeps{})
	list := httptest.NewRecorder()
	handler.ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/api/v1/side-chats/protected", nil))
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), `"orphan":true`) || !strings.Contains(list.Body.String(), "old-side") {
		t.Fatal(list.Code, list.Body.String())
	}
	for _, secret := range []string{"nativeHome", "nativeId", "content", "launchConfig"} {
		if strings.Contains(list.Body.String(), secret) {
			t.Fatal("private data in list", secret)
		}
	}
	closed := httptest.NewRecorder()
	handler.ServeHTTP(closed, httptest.NewRequest(http.MethodDelete, "/api/v1/side-chats/protected/old-side", nil))
	if closed.Code != http.StatusNoContent || svc.closed != "old-side" || svc.explicit != 0 || svc.cleanup != 0 {
		t.Fatal(closed.Code, svc)
	}
	explicit := httptest.NewRecorder()
	handler.ServeHTTP(explicit, httptest.NewRequest(http.MethodDelete, "/api/v1/sessions/session/conversation/side-chats/side", nil))
	if explicit.Code != http.StatusNoContent || svc.explicit != 1 || svc.cleanup != 1 {
		t.Fatal(explicit.Code, svc)
	}
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		req := httptest.NewRequest(method, "/api/v1/side-chats/protected", nil)
		if method == http.MethodDelete {
			req = httptest.NewRequest(method, "/api/v1/side-chats/protected/old-side", nil)
		}
		req = req.WithContext(requestscope.WithLAN(req.Context()))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != http.StatusNotFound {
			t.Fatal("LAN protected control", method, response.Code)
		}
	}
}

func TestSideChatHTTPDistinguishesUnavailableNativeHome(t *testing.T) {
	svc := &sideHTTPService{Service: chatsvc.New(chatsvc.Options{}), compactErr: chatsvc.ErrNativeHomeUnavailable}
	response := httptest.NewRecorder()
	sideHTTPRouter(svc).ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v1/sessions/session/conversation/side-chats/side/compact", nil))
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "CHAT_NATIVE_HOME_UNAVAILABLE") || strings.Contains(response.Body.String(), "CHAT_NATIVE_CONVERSATION_PROTECTED") {
		t.Fatal(response.Code, response.Body.String())
	}
}

func TestSideChatHTTPDisplayOmitsFrozenTurnsWithoutMutatingDelivery(t *testing.T) {
	for _, kind := range []string{"shared", "diagram", "omitted", "no-turn", "legacy", "empty"} {
		t.Run(kind, func(t *testing.T) {
			encode := func(value any) []byte {
				t.Helper()
				encoded, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				return encoded
			}
			captured := &domain.ChatExcerptSnapshot{
				Version: 1, ReferenceFormat: "numbered-v1", RawText: "**缓存**", CaptureMode: "exact", SourceTurnID: "source-turn",
				SourceRange:   &domain.ChatExcerptRange{Start: 20, End: 26, OffsetUnit: "utf-16"},
				RenderedRange: &domain.ChatExcerptRange{Start: 4, End: 6, OffsetUnit: "utf-16"}, Prefix: "the ", Suffix: " rest",
				Turn: &domain.ChatExcerptTurnSnapshot{ID: "source-turn", Truncated: true, Messages: []domain.ChatExcerptTurnMessage{
					{Role: "user", Text: "FROZEN_QUESTION\n" + strings.Repeat("问", 1000)},
					{Role: "assistant", Text: "FROZEN_ANSWER\n" + strings.Repeat("答", 1000)},
				}},
			}
			selection := "缓存"
			switch kind {
			case "diagram":
				selection = ""
				captured.RawText = "```mermaid\nflowchart LR; A-->B\n```"
				captured.CaptureMode, captured.ContainsFlowchart, captured.BadgeOnly = "block", true, true
				captured.SourceRange = &domain.ChatExcerptRange{Start: 0, End: len(captured.RawText), OffsetUnit: "utf-16"}
				captured.DiagramRange = captured.SourceRange
			case "omitted":
				captured.Turn, captured.ContextOmitted = nil, true
			case "no-turn":
				captured.Turn = nil
			case "legacy":
				captured = nil
			}
			frozen := domain.SideTurn{ID: "turn", SideID: "side", ClientMessageID: "receipt", Text: "Explain", State: "queued"}
			refs := make([]ports.ChatExcerptReference, 0)
			if kind != "empty" {
				for i := range 8 {
					frozen.References = append(frozen.References, domain.SideReference{ID: fmt.Sprintf("excerpt-%d", i), Order: int64(i), ConversationID: "main", MessageID: "deleted-source", Selection: selection, Snapshot: captured})
					refs = append(refs, ports.ChatExcerptReference{ID: fmt.Sprintf("excerpt-%d", i), Order: int64(i), ConversationID: "main", MessageID: "deleted-source", Text: selection, Snapshot: captured})
				}
			}
			snapshot := domain.SideSnapshot{Side: domain.SideConversation{ID: "side"}, Turns: []domain.SideTurn{frozen}, HasMore: true}
			svc := &sideHTTPService{Service: chatsvc.New(chatsvc.Options{}), turn: &frozen, snapshot: &snapshot}
			handler := sideHTTPRouter(svc)
			original := encode(snapshot)
			want := frozen
			if frozen.References != nil {
				want.References = append([]domain.SideReference(nil), frozen.References...)
				for i := range want.References {
					if captured != nil {
						snapshotCopy := *captured
						snapshotCopy.Turn = nil
						want.References[i].Snapshot = &snapshotCopy
					}
				}
			}
			request := encode(map[string]any{"text": "Explain", "clientMessageId": "receipt", "references": refs})
			for _, method := range []string{http.MethodPost, http.MethodPost, http.MethodGet, http.MethodGet} {
				path := "/api/v1/sessions/session/conversation/side-chats/side"
				status := http.StatusOK
				if method == http.MethodPost {
					path += "/messages"
					status = http.StatusAccepted
				} else {
					path += "?before=2026-11-01T00:00:00Z&limit=1"
				}
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, httptest.NewRequest(method, path, bytes.NewReader(request)))
				if response.Code != status {
					t.Fatalf("%s: %d %s", method, response.Code, response.Body.String())
				}
				var body struct {
					Turn     domain.SideTurn     `json:"turn"`
					Snapshot domain.SideSnapshot `json:"snapshot"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				got := body.Turn
				if method == http.MethodGet {
					if len(body.Snapshot.Turns) != 1 || !body.Snapshot.HasMore || body.Snapshot.Side.ID != "side" || svc.limit != 1 || svc.before.Format(time.RFC3339) != "2026-11-01T00:00:00Z" {
						t.Fatalf("pagination changed: %+v", body.Snapshot)
					}
					got = body.Snapshot.Turns[0]
				} else if !reflect.DeepEqual(svc.sent.Excerpts, refs) {
					t.Fatal("display filtering changed the captured service input")
				}
				turnFields := bytes.Count(response.Body.Bytes(), []byte(`"turn":`))
				if (method == http.MethodGet && turnFields != 0) || (method == http.MethodPost && turnFields != 1) {
					t.Fatal("display snapshots must omit the turn property entirely")
				}
				if !bytes.Equal(encode(got), encode(want)) || bytes.Contains(response.Body.Bytes(), []byte("FROZEN_")) {
					t.Fatalf("display must omit only background turns: %s", response.Body)
				}
				if !bytes.Equal(encode(snapshot), original) {
					t.Fatal("response mutated shared delivery snapshots")
				}
				if kind == "shared" {
					removed := len(encode(frozen)) - len(encode(got))
					if removed <= 48000 {
						t.Fatalf("background payload was not removed: %d bytes", removed)
					}
					t.Logf("side turn JSON: %d -> %d bytes (%d removed)", len(encode(frozen)), len(encode(got)), removed)
				}
			}
		})
	}
}
