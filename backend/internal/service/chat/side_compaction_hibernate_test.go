//go:build linux

package chat_test

import (
	"encoding/json"
	"errors"
	"os/exec"
	"syscall"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/processscope"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
)

func TestUpdateHibernateWithHistoricalSide(t *testing.T) {
	for _, ended := range []bool{false, true} {
		name := "live_scope"
		if ended {
			name = "previous_boot"
		}
		t.Run(name, func(t *testing.T) {
			h, conv := settledHibernationHarness(t, domain.TurnStateCompleted)
			// The scope describes an original launch. Boot change proves it ended;
			// the same-boot control still has a live host and must veto the update.
			child := exec.Command("sleep", "60")
			child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if err := child.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
			scope := processscope.Capture(child.Process.Pid)
			if scope == nil {
				t.Fatal("scope unavailable")
			}
			if ended {
				scope.BootID = "previous-boot"
			}
			side := domain.SideConversation{ID: "update-side", MainConversationID: "parent-conversation", SessionID: testSession, AppRunID: "old-launch", ProviderForkID: "native-side", ProviderHostID: "btw-old-launch-update-side", Harness: domain.HarnessClaudeCode, State: "ready", Generation: "g", PolicyVersion: 1}
			unverified := true
			content, _ := json.Marshal(map[string]any{"RunID": "old-launch", "Record": chatsvc.SideRecoveryRecord{Side: side, ProviderHostID: "btw-old-launch-update-side", ProviderForkID: "native-side", Harness: domain.HarnessClaudeCode, Generation: "g", PolicyVersion: 1, CompactionID: "update-compact", CompactionEpoch: "epoch", OutboundUnverified: &unverified}})
			row := domain.SideCompaction{ID: "update-compact", SideID: side.ID, SessionID: testSession, NativeHome: t.TempDir(), NativeID: "native-side", Provider: domain.HarnessClaudeCode, Scope: scope, State: domain.SideCompactionPending, Epoch: "epoch", Content: content}
			if err := h.st.RegisterSideCompaction(t.Context(), row); err != nil {
				t.Fatal(err)
			}
			if err := h.st.TransitionSideCompaction(t.Context(), row.ID, row.Epoch, domain.SideCompactionUnknown, false, false); err != nil {
				t.Fatal(err)
			}
			stopped, err := h.svc.HibernateChatForUpdate(t.Context(), testSession)
			if ended {
				if err != nil || !stopped || conv.calls.Load() != 1 {
					t.Fatalf("stopped=%v err=%v calls=%d", stopped, err, conv.calls.Load())
				}
			} else if !errors.Is(err, ports.ErrChatShutdownNotSettled) || stopped || conv.calls.Load() != 0 {
				t.Fatalf("stopped=%v err=%v calls=%d", stopped, err, conv.calls.Load())
			}
			rows, err := h.st.SideCompactions(t.Context(), true)
			if err != nil || len(rows) != 1 || rows[0].Historical != ended || rows[0].State != domain.SideCompactionUnknown {
				t.Fatalf("rows=%+v err=%v", rows, err)
			}
		})
	}
}
