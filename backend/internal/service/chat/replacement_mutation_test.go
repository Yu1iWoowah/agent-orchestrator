package chat_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type replacementSwitchCatalog struct{ calls int }

func (c *replacementSwitchCatalog) ModelCatalog(context.Context, string, string, bool) (ports.AgentModelCatalog, error) {
	c.calls++
	return ports.AgentModelCatalog{}, nil
}

func TestReplacementFencesProviderSwitchHTTPBeforeCatalogOrRuntime(t *testing.T) {
	h, _, _, _ := replacementControlHarness(t)
	ctx := t.Context()
	queued, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{
		Text: "preserve queued input", ClientMessageID: "existing-queued",
	})
	if err != nil || queued.State != domain.TurnStateQueued {
		t.Fatalf("prepare queued input: %+v %v", queued, err)
	}
	before, err := h.svc.Snapshot(ctx, testSession)
	if err != nil {
		t.Fatal(err)
	}
	catalog := &replacementSwitchCatalog{}
	h.svc.SetSwitchModelCatalog(catalog)
	router := chi.NewRouter()
	(&controllers.ConversationsController{Svc: h.svc}).Register(router)
	for _, body := range []string{
		`{"targetHarness":"codex","model":"candidate"}`,
		`{"targetHarness":"codex","model":"candidate","message":{"text":"continue","clientMessageId":"new-id"}}`,
		`{"targetHarness":"claude-code","model":"next-model"}`,
		`{"targetHarness":"codex","model":"candidate","force":true}`,
	} {
		resp := httptest.NewRecorder()
		router.ServeHTTP(resp, httptest.NewRequest(http.MethodPost, "/sessions/p1-1/provider-switch", strings.NewReader(body)))
		if resp.Code != http.StatusConflict || !strings.Contains(resp.Body.String(), "ORCHESTRATOR_REPLACEMENT_INTAKE_FENCED") {
			t.Fatalf("%s: got %d %s", body, resp.Code, resp.Body)
		}
	}
	after, err := h.svc.Snapshot(ctx, testSession)
	if err != nil || !reflect.DeepEqual(before, after) || catalog.calls != 0 || len(h.conv.sentTexts()) != 0 {
		t.Fatalf("fenced switch changed state or used providers: catalog=%d sends=%v err=%v", catalog.calls, h.conv.sentTexts(), err)
	}
}

func TestReplacementFencesPublicChatMutationsAndPreservesQueuedReceipt(t *testing.T) {
	for _, action := range []string{"steer", "steer or send", "promote", "rollback", "fork", "edit", "activate branch", "switch provider", "switch and send", "interrupt"} {
		t.Run(action, func(t *testing.T) {
			h, _, _, _ := replacementControlHarness(t)
			ctx := context.Background()
			queued, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{Text: "preserve queued input", ClientMessageID: "queued-input", Content: []ports.ChatContent{{Type: "image", Data: "aW1hZ2U=", MIMEType: "image/png"}}})
			if err != nil || queued.State != domain.TurnStateQueued {
				t.Fatalf("ordinary input not held: %+v %v", queued, err)
			}
			msg := ports.ChatUserMessage{Text: "new guidance", ClientMessageID: "mutation-id"}
			switch action {
			case "steer":
				_, err = h.svc.Steer(ctx, testSession, msg)
			case "steer or send":
				_, err = h.svc.SteerOrSend(ctx, testSession, msg, false)
			case "promote":
				_, err = h.svc.PromoteQueuedTurn(ctx, testSession, queued.ID)
			case "rollback":
				_, err = h.svc.Rollback(ctx, testSession, queued.ID, "")
			case "fork":
				_, err = h.svc.ForkConversation(ctx, testSession)
			case "edit":
				_, err = h.svc.EditMessage(ctx, testSession, queued.ID, msg)
			case "activate branch":
				_, err = h.svc.ActivateBranch(ctx, testSession, "other-branch")
			case "switch provider":
				err = h.svc.SwitchProvider(ctx, testSession, domain.ProviderSwitchRequest{TargetHarness: domain.HarnessClaudeCode})
			case "switch and send":
				_, _, err = h.svc.SwitchProviderAndSend(ctx, testSession, domain.ProviderSwitchRequest{TargetHarness: domain.HarnessClaudeCode}, msg)
			case "interrupt":
				err = h.svc.Interrupt(ctx, testSession)
			}
			if !errors.Is(err, domain.ErrReplacementIntakeFenced) {
				t.Fatalf("%s crossed replacement mutation fence: %v", action, err)
			}
			snapshot, err := h.svc.Snapshot(ctx, testSession)
			if err != nil || len(snapshot.Turns) != 1 || snapshot.Turns[0].ID != queued.ID || snapshot.Turns[0].State != domain.TurnStateQueued || len(snapshot.Messages) != 1 || snapshot.Messages[0].ClientMessageID != "queued-input" || snapshot.Messages[0].DeliveryContentJSON == "" {
				t.Fatalf("%s lost queued receipt/content: %+v err=%v", action, snapshot, err)
			}
		})
	}
}

type denyingReplacementMutationGate struct{ calls, releases int }

func (g *denyingReplacementMutationGate) BeginReplacementMutation(ctx context.Context, _ domain.SessionID) (context.Context, func(), error) {
	g.calls++
	return ctx, func() { g.releases++ }, domain.ErrReplacementIntakeFenced
}

func TestPublicChatMutationUsesManagerAdmissionBeforeControllerLocks(t *testing.T) {
	h, _, _, _ := replacementControlHarness(t)
	gate := &denyingReplacementMutationGate{}
	h.svc.SetReplacementMutationGate(gate)
	_, err := h.svc.ForkConversation(context.Background(), testSession)
	if !errors.Is(err, domain.ErrReplacementIntakeFenced) || gate.calls != 1 {
		t.Fatalf("manager gate bypassed: calls=%d err=%v", gate.calls, err)
	}
}
