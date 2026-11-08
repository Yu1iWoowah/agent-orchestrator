package store_test

import (
	"context"
	"encoding/json"
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
	usagesvc "github.com/aoagents/agent-orchestrator/backend/internal/service/usage"
)

func TestUsageEventsReadAPI(t *testing.T) {
	for _, h := range []domain.AgentHarness{domain.HarnessClaudeCode, domain.HarnessCodex} {
		for _, mode := range []domain.SessionMode{domain.SessionModeChat, domain.SessionModeTUI} {
			t.Run(string(h)+"/"+string(mode), func(t *testing.T) {
				s := newTestStore(t)
				ctx := context.Background()
				seedProject(t, s, "usage")
				rec := sampleRecord("usage")
				rec.Harness = h
				rec.Mode = mode
				rec.Metadata.AgentSessionID = "root"
				rec.Metadata.ProviderConversationID = "chat-root"
				sess, err := s.CreateSession(ctx, rec)
				mustNoError(t, err)
				now := time.Now().UTC()
				source := seedUsageSource(t, s, sess, now)
				childKind := domain.UsageSourceCodexRollout
				if h == domain.HarnessClaudeCode {
					childKind = domain.UsageSourceClaudeSubagent
				}
				child := mustInsertUsageSource(t, s, now, domain.UsageSourceRecord{BindingID: source.BindingID, Kind: childKind, NativeSessionID: "child", SubagentID: "child", ArtifactPath: "/fixture/child", Generation: 1, State: domain.UsageSourceError, LastErrorCode: domain.UsageErrorMalformedJSONL, AnomalyCount: 1})
				reader := usagesvc.NewEventsReader(s)
				router := httpd.NewRouterWithControl(config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, httpd.APIDeps{UsageEvents: reader}, httpd.ControlDeps{})
				request := func(path string, want int) []byte {
					t.Helper()
					w := httptest.NewRecorder()
					router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
					if w.Code != want {
						t.Fatalf("status=%d want=%d body=%s", w.Code, want, w.Body.String())
					}
					return w.Body.Bytes()
				}
				path := "/api/v1/usage/sessions/" + string(sess.ID) + "/events"
				empty := request(path, 200)
				if !strings.Contains(string(empty), `"events":[]`) || !strings.Contains(string(empty), `"sources":[{`) {
					t.Fatalf("empty=%s", empty)
				}
				events := []domain.ModelUsageEvent{usageEvent("first", canonicalUsageTokens(100, 70, 30, 5)), usageEvent("second", domain.UsageTokenMetrics{})}
				events[0].CreatedAt = now
				events[0].ProviderUsageJSON = `{"last_token_usage":{"input_tokens":100,"cached_input_tokens":70,"output_tokens":5,"secret":"SENSITIVE"},"path":"SENSITIVE"}`
				if h == domain.HarnessClaudeCode {
					events[0].ProviderID = domain.UsageProviderAnthropic
					events[0].ProviderUsageJSON = `{"input_tokens":10,"cache_creation_input_tokens":20,"cache_read_input_tokens":70,"output_tokens":5,"secret":"SENSITIVE"}`
				}
				events[1].ProviderUsageJSON = ""
				if h == domain.HarnessClaudeCode {
					events[1].ProviderID = domain.UsageProviderAnthropic
				}
				mustNoError(t, s.ApplyUsageChunk(ctx, source.ID, 0, source.UpdatedAt, domain.SourceCursorState{ByteOffset: 10, State: domain.UsageSourceActive, UpdatedAt: now}, events))
				before, err := s.LatestSeq(ctx)
				mustNoError(t, err)
				body := request(path+"?limit=1", 200)
				var first domain.UsageEventsPage
				mustNoError(t, json.Unmarshal(body, &first))
				if len(first.Events) != 1 || !first.HasMore || first.Events[0].InputTokens == nil || *first.Events[0].InputTokens != 100 || first.Mode != string(mode) || first.Bindings[0].Harness != string(h) || first.AccountID != nil {
					t.Fatalf("page=%s", body)
				}
				expectedNative := "root"
				if mode == domain.SessionModeChat {
					expectedNative = "chat-root"
				}
				if first.CurrentNativeID != expectedNative || len(first.Bindings[0].Sources) != 2 || first.Bindings[0].Sources[1].ID != child.ID || first.Bindings[0].Sources[1].AnomalyCount != 1 {
					t.Fatalf("metadata=%s", body)
				}
				for _, secret := range []string{"SENSITIVE", "artifactPath", "parserState", "providerUsageJson", "fileIdentity"} {
					if strings.Contains(string(body), secret) {
						t.Fatalf("leaked %s: %s", secret, body)
					}
				}
				next := request(fmt.Sprintf("%s?limit=1&throughId=%d&afterId=%d", path, first.ThroughID, first.NextAfterID), 200)
				var last domain.UsageEventsPage
				mustNoError(t, json.Unmarshal(next, &last))
				if len(last.Events) != 1 || last.HasMore || last.Events[0].InputTokens != nil || last.Events[0].CreatedAt != nil || last.ThroughID != first.ThroughID {
					t.Fatalf("last=%s", next)
				}
				after, err := s.LatestSeq(ctx)
				mustNoError(t, err)
				if after != before {
					t.Fatal("read mutated database")
				}
				for _, q := range []string{"?limit=0", "?limit=1001", "?limit=no", "?afterId=-1", "?afterId=1", "?throughId=-1", "?throughId=9223372036854775807", "?afterId=2&throughId=1", "?limit=1&limit=2", "?throughId="} {
					b := request(path+q, 400)
					if !strings.Contains(string(b), `"requestId":`) || !strings.Contains(string(b), `"code":"INVALID_USAGE_PAGE"`) {
						t.Fatalf("envelope=%s", b)
					}
				}
				request("/api/v1/usage/sessions/absent/events", 404)
				provider := domain.AgentAccountProviderCodex
				account := "device-codex"
				if h == domain.HarnessClaudeCode {
					provider = domain.AgentAccountProviderClaude
					account = "device-claude"
				}
				mustNoError(t, s.BindSessionAgentAccount(ctx, sess.ID, provider, account, now))
				annotated, err := reader.Events(ctx, sess.ID, 0, nil, 100)
				mustNoError(t, err)
				if annotated.AccountID == nil || *annotated.AccountID != account {
					t.Fatalf("account=%+v", annotated.AccountID)
				}
				// Later insertions are outside the original bound, while a fresh scan sees them.
				current, ok, err := s.GetUsageSourceForIngestion(ctx, source.ID)
				mustNoError(t, err)
				if !ok {
					t.Fatal("missing source")
				}
				events[0].SourceEventKey = "third"
				mustNoError(t, s.ApplyUsageChunk(ctx, source.ID, 10, current.Source.UpdatedAt, domain.SourceCursorState{ByteOffset: 20, State: domain.UsageSourceActive, UpdatedAt: now.Add(time.Second)}, []domain.ModelUsageEvent{events[0]}))
				bounded := request(fmt.Sprintf("%s?throughId=%d&afterId=%d", path, first.ThroughID, last.NextAfterID), 200)
				if !strings.Contains(string(bounded), `"events":[]`) {
					t.Fatalf("bound=%s", bounded)
				}
				fresh, err := reader.Events(ctx, sess.ID, 0, nil, 100)
				mustNoError(t, err)
				if len(fresh.Events) != 3 {
					t.Fatalf("fresh=%+v", fresh)
				}
			})
		}
	}
}
