package acp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/persistenthost"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestPersistentBackgroundRetentionSeparatesHistoryAndTrailingNoise(t *testing.T) {
	cfg, start := restartFixture(t, map[string]string{"AO_TEST_PERSISTENT_ACP_HISTORY_NOISE": "1"})
	cfg.Harness = domain.HarnessClaudeCode
	cfg.ClaudeBackgroundTurns = true
	cfg.Capabilities = ports.ChatCapabilities{ports.ChatCapabilityResume: true}
	cfg.SessionMeta = func(LaunchConfig) map[string]any { return map[string]any{"ao.retainUnpromptedEvents": true} }
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	t.Cleanup(func() { _ = persistenthost.Shutdown(context.Background(), start.DataDir, string(start.SessionID)) })
	opened, err := New(cfg, nil).Resume(ctx, ports.ChatResumeConfig{
		SessionID: start.SessionID, DataDir: start.DataDir, WorkspacePath: start.WorkspacePath,
		ProviderScopeID: start.ProviderScopeID, ProviderConversationID: "persistent-provider-session",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opened.Close() })
	loader := opened.(*refreshableConversation)
	history, err := loader.ReadHistory(ctx)
	if err != nil || len(history) == 0 {
		t.Fatalf("initial history: %+v %v", history, err)
	}
	for _, event := range history {
		if !strings.HasPrefix(event.ProviderEventID, "acp-history:") {
			t.Fatalf("history acquired a random live identity: %+v", event)
		}
	}
	for range 2 {
		again, err := loader.RefreshHistory(ctx)
		if err != nil {
			t.Fatal(err)
		}
		assertReplayHistoryEqual(t, history, again)
	}
	journal := filepath.Join(start.DataDir, "chat-hosts", string(start.SessionID), "acp-prompt.journal")
	assertEmptyJournals := func() bool {
		for _, path := range []string{journal, journal + ".unprompted"} {
			info, err := os.Stat(path)
			if err != nil || info.Size() != 0 {
				return false
			}
		}
		return true
	}
	if !assertEmptyJournals() {
		t.Fatal("session/load left transcript in host replay journals")
	}
	ref, err := loader.SendTurn(ctx, ports.ChatUserMessage{Text: "ordinary prompt"})
	if err != nil {
		t.Fatal(err)
	}
	if err := loader.StartDeferredTurn(ref.ProviderTurnID); err != nil {
		t.Fatal(err)
	}
	checkpoints, terminal := 0, false
	for checkpoints < 2 || !terminal {
		select {
		case event := <-loader.Events():
			if event.Kind == ports.ChatEventProviderCheckpoint {
				checkpoints++
			}
			if event.Kind == ports.ChatEventTurnCompleted {
				terminal = true
			}
			if err := loader.AcknowledgeProviderEvent(ctx, event.ProviderEventID); err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatalf("trailing notifications never reached the commit boundary: checkpoints=%d terminal=%v", checkpoints, terminal)
		}
	}
	for !assertEmptyJournals() {
		select {
		case <-ctx.Done():
			t.Fatal("prompt ACK and noise checkpoints did not release journals")
		case <-time.After(5 * time.Millisecond):
		}
	}
}
