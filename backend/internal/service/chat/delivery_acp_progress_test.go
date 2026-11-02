package chat_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/acp"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/persistenthost"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
)

func TestACPProgressProviderHelper(t *testing.T) {
	if os.Getenv("AO_TEST_ACP_PROGRESS") != "1" {
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(scanner.Bytes(), &request) != nil {
			continue
		}
		switch request.Method {
		case "initialize":
			fmt.Fprintf(os.Stdout, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":1,"agentCapabilities":{},"authMethods":[]}}`+"\n", request.ID)
		case "session/new":
			fmt.Fprintf(os.Stdout, `{"jsonrpc":"2.0","id":%s,"result":{"sessionId":"progress-session"}}`+"\n", request.ID)
		case "session/prompt":
			for step := 1; step <= 4; step++ {
				deadline := time.Now().Add(time.Minute)
				for {
					raw, _ := os.ReadFile(os.Getenv("AO_TEST_ACP_PROGRESS_CONTROL"))
					n, _ := strconv.Atoi(string(raw))
					if n >= step {
						break
					}
					if time.Now().After(deadline) {
						os.Exit(2)
					}
					time.Sleep(time.Millisecond)
				}
				if step == 4 {
					fmt.Fprintf(os.Stdout, `{"jsonrpc":"2.0","id":%s,"result":{"stopReason":"end_turn"}}`+"\n", request.ID)
					break
				}
				fmt.Fprintln(os.Stdout, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"progress-session","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"x"}}}}`)
			}
		}
	}
	os.Exit(0)
}

func TestACPStreamingBeyondDeliveryDeadlineDoesNotCreateAttention(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	st := openStore(t)
	dataDir := t.TempDir()
	control := filepath.Join(dataDir, "progress")
	cfg := acp.Config{Harness: domain.HarnessOpenCode, Capabilities: ports.ChatCapabilities{ports.ChatCapabilityStreaming: true, ports.ChatCapabilityApprovals: true, ports.ChatCapabilityInterrupt: true, ports.ChatCapabilityResume: true}, Probe: func(context.Context) error { return nil }, Launch: func(context.Context, acp.LaunchConfig) (acp.Launch, error) {
		return acp.Launch{Command: os.Args[0], Args: []string{"-test.run=^TestACPProgressProviderHelper$"}, Env: map[string]string{"AO_TEST_ACP_PROGRESS": "1", "AO_TEST_ACP_PROGRESS_CONTROL": control}}, nil
	}}
	logger := slog.New(slog.DiscardHandler)
	var ids atomic.Uint64
	svc := chatsvc.New(chatsvc.Options{Store: st, Reader: fullSnapshotReader(st), Sessions: st, Drivers: fakeRegistry{driver: acp.New(cfg, logger)}, Activity: &recordingActivity{}, Log: logger, NewID: func() string { return fmt.Sprintf("progress-%d", ids.Add(1)) }, DataDir: dataDir})
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 6*time.Second)
		defer stopCancel()
		_ = svc.StopAll(stopCtx)
		if err := persistenthost.Shutdown(stopCtx, dataDir, string(testSession)); err != nil {
			t.Errorf("host cleanup: %v", err)
		}
	})
	c, err := svc.Start(ctx, chatsvc.StartConfig{SessionID: testSession, ProjectID: testProject, Kind: domain.KindOrchestrator, Harness: domain.HarnessOpenCode, DataDir: dataDir, WorkspacePath: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := svc.Send(ctx, testSession, ports.ChatUserMessage{Text: "continue streaming", ClientMessageID: "long-acp-input"})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	for step := 1; step <= 3; step++ {
		if step > 1 {
			select {
			case <-time.After(10 * time.Second):
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
		if err := os.WriteFile(control, []byte(strconv.Itoa(step)), 0600); err != nil {
			t.Fatal(err)
		}
		awaitACPReplay(ctx, t, "native output projected", func() bool {
			s, e := st.LoadConversationSnapshot(ctx, c.ConversationID())
			return e == nil && len(s.Messages) == 2 && len(s.Messages[1].Text) == step
		})
	}
	// Both processes use the OS clock. Exercise the real 30s silence query with
	// native chunks at ~0/10/20s, rather than moving only the daemon clock.
	select {
	case <-time.After(time.Until(started.Add(31 * time.Second))):
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	awaitACPReplay(ctx, t, "native progress queried", func() bool {
		a, e := st.ChatDeliveryAttemptForTurn(ctx, testSession, turn.ID)
		return e == nil && a.Receipt != nil && a.Receipt.ProgressSequence > 0
	})
	streaming, err := st.ChatDeliveryAttemptForTurn(ctx, testSession, turn.ID)
	if err != nil || streaming.AttentionID != "" || streaming.BlockedSince != nil || streaming.Disposition != domain.ChatDeliveryUnknown || streaming.EvidenceLevel != 0 || !streaming.LastProgressAt.Equal(streaming.Receipt.ObservedAt) {
		t.Fatalf("streaming ACP misclassified or observation changed native time: %+v %v", streaming, err)
	}
	if err := os.WriteFile(control, []byte("4"), 0600); err != nil {
		t.Fatal(err)
	}
	awaitACPReplay(ctx, t, "ACP execution settled", func() bool {
		a, e := st.ChatDeliveryAttemptForTurn(ctx, testSession, turn.ID)
		return e == nil && a.ExecutionSettled
	})
	a, err := st.ChatDeliveryAttemptForTurn(ctx, testSession, turn.ID)
	if err != nil || a.AttentionID != "" || a.ErrorCode != "" || a.Disposition != domain.ChatDeliveryAccepted {
		t.Fatalf("ACP terminal receipt did not settle: %+v %v", a, err)
	}
	if err := svc.StopAll(ctx); err != nil {
		t.Fatalf("completed ACP cannot shut down: %v", err)
	}
}
