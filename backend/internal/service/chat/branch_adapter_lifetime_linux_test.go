//go:build linux

package chat_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/persistenthost"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/processscope"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

func TestEditedBranchAdapterSettlesAfterDelayedExit(t *testing.T) {
	const epoch = "replacement-adapter"
	dataDir := t.TempDir()
	source := newFakeConversation()
	caps := productionCaps()
	caps[ports.ChatCapabilityPromptReplay] = true
	caps[ports.ChatCapabilityEmbeddedContext] = true
	source.setCapabilities(caps)
	replacement := &terminatingConversation{fakeConversation: newFakeConversation()}
	replacement.providerConversationID = "edited-thread"
	replacement.turnSeq = 100
	replacement.outboundEpoch, replacement.deliveryEpoch = epoch, epoch
	replacement.setCapabilities(caps)
	started := false
	h := newHarnessWithConversationAndStoreForHarness(t, source, func(st *store.Store) chatsvc.Store { return st },
		domain.HarnessClaudeCode, func(opts *chatsvc.Options) {
			opts.Drivers = fakeRegistry{driver: fakeDriver{conv: source, start: func(ports.ChatStartConfig) (ports.ChatConversation, error) {
				if !started {
					started = true
					return source, nil
				}
				return replacement, nil
			}}}
			opts.BindAdapterOwner = func(id domain.SessionID, epoch, conversation, branch, generation string) error {
				return persistenthost.BindAdapterOwner(dataDir, id, epoch, conversation, branch, generation)
			}
		})
	h.conv = source
	original := completeTurn(t, h, "original", "provider-turn-1")
	h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		return len(s.Turns) == 1 && s.Turns[0].State == domain.TurnStateCompleted
	})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := h.svc.DrainQueued(ctx, testSession); err != nil {
		t.Fatal(err)
	}
	adapter := exec.CommandContext(ctx, "sh", "-c", "read -r release || :")
	adapter.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	release, err := adapter.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := adapter.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = release.Close(); _ = adapter.Wait() })
	scope := processscope.Capture(adapter.Process.Pid)
	if scope == nil {
		t.Fatal("could not capture the replacement adapter identity")
	}
	hostDir := filepath.Join(dataDir, "chat-hosts", string(testSession))
	if err := os.MkdirAll(hostDir, 0700); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(struct {
		Epoch string
		Scope *processscope.Scope
	}{epoch, scope})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hostDir, "adapter.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	replacement.onSend = func(string) {
		owner, err := h.st.SessionAgentSettings(ctx, testSession)
		if err != nil {
			t.Errorf("read durable Edit owner before dispatch: %v", err)
			return
		}
		record := filepath.Join(hostDir, "adapter-owner-"+owner.OwnerGeneration+".json")
		if _, err := os.Stat(record); err != nil {
			t.Errorf("adapter owner was not recorded before private Edit dispatch: %v", err)
		}
	}
	edited, err := h.svc.EditMessage(ctx, testSession, original, ports.ChatUserMessage{Text: "edited", ClientMessageID: "edit-adapter"})
	if err != nil {
		t.Fatal(err)
	}
	controller, err := h.svc.Controller(testSession)
	if err != nil || controller.Generation() == h.ctrl.Generation() {
		t.Fatalf("Edit did not publish a replacement generation: %v", err)
	}
	replacement.emit(ports.ChatEvent{
		Kind: ports.ChatEventApprovalRequested, ProviderTurnID: edited.Turn.ProviderTurnID,
		RequestID: "edit-approval", ProviderItemID: "edit-approval", ActivityStatus: domain.ActivityStatusPending,
	})
	pending := h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		return len(s.Activities) == 1 && s.Activities[0].Status == domain.ActivityStatusPending
	})
	approvalItem := pending.Activities[0].ProviderItemID
	replacement.emit(ports.ChatEvent{
		Kind: ports.ChatEventControllerState, ControllerState: ports.ChatControllerStopped, Err: ports.ErrChatRecoveryInconclusive,
	})
	if err := replacement.Close(); err != nil {
		t.Fatal(err)
	}
	controller.Wait()

	sweep := func(wantTurn domain.TurnState, wantApproval domain.ActivityStatus) {
		t.Helper()
		if err := persistenthost.ReconcileExitedAdapters(ctx, dataDir, func(domain.SessionID, string) bool {
			return !h.svc.HasLiveChatController(testSession)
		}, func(ctx context.Context, id domain.SessionID, conversation, branch, generation string) error {
			return h.st.SettleExitedChatAdapter(ctx, id, conversation, branch, generation, h.now())
		}); err != nil {
			t.Fatal(err)
		}
		row, err := h.st.TurnByID(ctx, edited.Turn.ID)
		if err != nil || row.State != wantTurn {
			t.Fatalf("edited turn after sweep = %s, %v; want %s", row.State, err, wantTurn)
		}
		snap, err := h.st.LoadConversationSnapshot(ctx, edited.Turn.ConversationID)
		if err != nil {
			t.Fatal(err)
		}
		if snap.ActiveBranch.ID != edited.ActiveBranchID {
			t.Fatalf("adapter sweep changed the active branch to %q", snap.ActiveBranch.ID)
		}
		oldTurn, err := h.st.TurnByID(ctx, original)
		if err != nil || oldTurn.State != domain.TurnStateCompleted || oldTurn.ProviderTurnID != "provider-turn-1" {
			t.Fatalf("adapter sweep changed the original branch's turn: %s/%q, %v", oldTurn.State, oldTurn.ProviderTurnID, err)
		}
		if got := findActivity(t, snap, approvalItem).Status; got != wantApproval {
			t.Fatalf("edited approval after sweep = %s, want %s", got, wantApproval)
		}
	}
	sweep(domain.TurnStateRunning, domain.ActivityStatusPending)
	if err := release.Close(); err != nil {
		t.Fatal(err)
	}
	if err := adapter.Wait(); err != nil {
		t.Fatal(err)
	}
	sweep(domain.TurnStateInterrupted, domain.ActivityStatusFailed)
	if calls := replacement.sendCallCount(); calls != 1 {
		t.Fatalf("original edit sent %d times, want once", calls)
	}
}
