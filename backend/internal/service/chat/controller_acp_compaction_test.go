package chat_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/acp"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/persistenthost"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
)

func TestACPCompactionErrorProviderHelper(t *testing.T) {
	path := os.Getenv("AO_TEST_CHAT_ACP_COMPACTION_LOG")
	if path == "" {
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Prompt []struct{ Text string } `json:"prompt"`
			} `json:"params"`
		}
		if json.Unmarshal(scanner.Bytes(), &request) != nil {
			continue
		}
		switch request.Method {
		case "initialize":
			_, _ = fmt.Fprintf(os.Stdout, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":1,"agentCapabilities":{"sessionCapabilities":{"resume":{}}},"authMethods":[]}}`+"\n", request.ID)
		case "session/new", "session/resume", "session/load":
			_, _ = fmt.Fprintf(os.Stdout, `{"jsonrpc":"2.0","id":%s,"result":{"sessionId":"compact-provider"}}`+"\n", request.ID)
		case "session/prompt":
			compact := len(request.Params.Prompt) > 0 && request.Params.Prompt[0].Text == "/compact"
			kind := "input"
			if compact {
				kind = "compact"
			}
			f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
			if err != nil {
				os.Exit(2)
			}
			_, _ = fmt.Fprintln(f, kind)
			_ = f.Close()
			if compact {
				_, _ = fmt.Fprintf(os.Stdout, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32603,"message":"ordinary ACP RPC error"}}`+"\n", request.ID)
			} else {
				_, _ = fmt.Fprintf(os.Stdout, `{"jsonrpc":"2.0","id":%s,"result":{"stopReason":"end_turn"}}`+"\n", request.ID)
			}
		case "session/close":
			if len(request.ID) > 0 {
				_, _ = fmt.Fprintf(os.Stdout, `{"jsonrpc":"2.0","id":%s,"result":{}}`+"\n", request.ID)
			}
		}
	}
	os.Exit(0)
}

func TestACPUnknownCompactionRequiresExplicitExitAcrossReconnect(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	st := openStore(t)
	dataDir, workspace := t.TempDir(), t.TempDir()
	requests := filepath.Join(dataDir, "requests")
	logger := slog.New(slog.DiscardHandler)
	cfg := acp.Config{
		Harness: domain.HarnessOpenCode,
		Capabilities: ports.ChatCapabilities{
			ports.ChatCapabilityStreaming: true, ports.ChatCapabilityResume: true,
			ports.ChatCapabilityApprovals: true, ports.ChatCapabilityInterrupt: true, ports.ChatCapabilityCompaction: true,
		},
		Probe: func(context.Context) error { return nil },
		Launch: func(context.Context, acp.LaunchConfig) (acp.Launch, error) {
			return acp.Launch{Command: os.Args[0], Args: []string{"-test.run=^TestACPCompactionErrorProviderHelper$"}, Env: map[string]string{"AO_TEST_CHAT_ACP_COMPACTION_LOG": requests}}, nil
		},
	}
	var ids atomic.Uint64
	newService := func() *chatsvc.Service {
		svc := chatsvc.New(chatsvc.Options{Store: st, Reader: fullSnapshotReader(st), Sessions: st,
			Drivers: fakeRegistry{driver: acp.New(cfg, logger)}, Activity: &recordingActivity{}, Log: logger, DataDir: dataDir,
			NewID: func() string { return fmt.Sprintf("compact-%d", ids.Add(1)) },
		})
		t.Cleanup(func() {
			stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer stopCancel()
			_ = svc.Stop(stopCtx, testSession)
		})
		return svc
	}
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 6*time.Second)
		defer stopCancel()
		if err := persistenthost.Shutdown(stopCtx, dataDir, string(testSession)); err != nil {
			t.Errorf("host cleanup: %v", err)
		}
	})
	start := chatsvc.StartConfig{SessionID: testSession, ProjectID: testProject, Kind: domain.KindOrchestrator,
		Harness: domain.HarnessOpenCode, DataDir: dataDir, WorkspacePath: workspace, HistoryMode: ports.ChatHistoryDeferred}
	firstService := newService()
	first, err := firstService.Start(ctx, start)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Compact(ctx); err != nil {
		t.Fatal(err)
	}
	awaitACPReplay(ctx, t, "unknown compaction attention", func() bool {
		page, err := st.ListAttentionQuestions(ctx, testSession, domain.AttentionQuestionQuery{State: "open"}, time.Now())
		return err == nil && page.Open == 1 && strings.Contains(page.Items[0].Payload.Explanation, "Exit agent") && strings.Contains(page.Items[0].Payload.Explanation, "訊息")
	})
	queued, err := first.Send(ctx, ports.ChatUserMessage{Text: "preserve this queued question"})
	if err != nil || queued.State != domain.TurnStateQueued || first.State() != ports.ChatControllerBusy {
		t.Fatalf("unknown compaction lost visible admission fence: %+v %v state=%s", queued, err, first.State())
	}
	if err := first.Interrupt(ctx); !errors.Is(err, ports.ErrChatDeliveryUnverified) {
		t.Fatalf("interrupt manufactured execution settlement: %v", err)
	}
	events, err := st.ProviderEventsSince(ctx, first.ConversationID(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	unknownID, unknownPayload := "", ""
	for _, event := range events {
		var record struct{ Compaction *ports.ChatCompactionReceipt }
		if json.Unmarshal([]byte(event.PayloadJson), &record) == nil && record.Compaction != nil && record.Compaction.Outcome == ports.ChatCompactionUnknown {
			unknownID, unknownPayload = event.ProviderEventID, event.PayloadJson
		}
	}
	if unknownID == "" {
		t.Fatal("unknown outcome was not durably archived")
	}
	hostBefore := readACPReplayHost(t, dataDir)
	if err := chatsvc.DetachIncompleteForTest(ctx, first); err != nil {
		t.Fatal(err)
	}
	secondService := newService()
	start.ProviderConversationID, start.ReconnectOnly = first.ProviderConversationID(), true
	second, err := secondService.Start(ctx, start)
	if err != nil {
		t.Fatalf("unknown compaction reconnect: %v", err)
	}
	if host := readACPReplayHost(t, dataDir); host.PID != hostBefore.PID || !host.StartedAt.Equal(hostBefore.StartedAt) {
		t.Fatal("unknown compaction replaced its original host")
	}
	if second.State() != ports.ChatControllerBusy {
		t.Fatal("reconnect forgot the durable unknown fence")
	}
	if err := secondService.PreflightShutdown(ctx); !errors.Is(err, ports.ErrChatShutdownNotSettled) {
		t.Fatalf("unknown compaction passed normal shutdown: %v", err)
	}
	if err := secondService.Stop(ctx, testSession); err != nil {
		t.Fatalf("explicit user Exit was unreachable: %v", err)
	}
	if page, err := st.ListAttentionQuestions(ctx, testSession, domain.AttentionQuestionQuery{State: "open"}, time.Now()); err != nil || page.Open != 0 {
		t.Fatalf("explicit Exit left its attention open: %+v %v", page, err)
	}
	retained, err := st.TurnByID(ctx, queued.ID)
	if err != nil || retained.State != domain.TurnStateFailed || retained.ID != queued.ID {
		t.Fatalf("explicit Exit lost queued input: %+v %v", retained, err)
	}
	rows, err := st.LoadConversationSnapshot(ctx, first.ConversationID())
	if err != nil {
		t.Fatal(err)
	}
	preserved := false
	for _, message := range rows.Messages {
		preserved = preserved || message.TurnID == queued.ID && message.Text == "preserve this queued question"
	}
	if !preserved {
		t.Fatal("explicit Exit discarded the queued question content")
	}
	thirdService := newService()
	start.ReconnectOnly = false
	third, err := thirdService.Start(ctx, start)
	if err != nil {
		t.Fatalf("explicit user Resume: %v", err)
	}
	if content, _ := os.ReadFile(requests); string(content) != "compact\n" {
		t.Fatalf("Exit/Resume automatically replayed work: %s", content)
	}
	turn, err := third.Send(ctx, ports.ChatUserMessage{Text: "new question after explicit Exit and Resume"})
	if err != nil {
		t.Fatal(err)
	}
	awaitACPReplay(ctx, t, "new explicitly submitted input completed", func() bool {
		row, err := st.TurnByID(ctx, turn.ID)
		return err == nil && row.State == domain.TurnStateCompleted
	})
	if content, _ := os.ReadFile(requests); string(content) != "compact\ninput\n" {
		t.Fatalf("unknown compact or queued input was replayed: %s", content)
	}
	events, err = st.ProviderEventsSince(ctx, first.ConversationID(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.ProviderEventID == unknownID && event.PayloadJson == unknownPayload {
			return
		}
	}
	t.Fatal("explicit Exit/Resume rewrote the original unknown outcome")
}
