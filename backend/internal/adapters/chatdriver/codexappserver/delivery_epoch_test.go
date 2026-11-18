package codexappserver

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// The chat controller settles an unknown attempt left by a host from before a
// reboot only through ports.ChatDeliveryEpochJudge. Codex reports its host
// epoch, so the real conversation type must judge it too; the shared service
// fake implements both and cannot catch a driver that misses one.
func TestCodexConversationJudgesPreviousBootDeliveryEpochs(t *testing.T) {
	var conv any = &conversation{}
	if _, ok := conv.(ports.ChatDeliveryEpochReporter); !ok {
		t.Fatal("codex conversation no longer reports its delivery epoch")
	}
	judge, ok := conv.(ports.ChatDeliveryEpochJudge)
	if !ok {
		t.Fatal("codex conversation reports delivery epochs but cannot judge them; a previous boot's unknown attempt would never settle")
	}
	if runtime.GOOS != "linux" {
		t.Skip("boot identity is recorded on Linux only")
	}
	raw, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		t.Skip("boot identity is unavailable")
	}
	current := strings.TrimSpace(string(raw))
	previous := "00000000-0000-4000-8000-000000000001"
	if previous == current {
		previous = "00000000-0000-4000-8000-000000000002"
	}
	started := time.Now().UnixNano()
	if !judge.DeliveryEpochFromPreviousBoot(fmt.Sprintf("host-4242-%d-%s", started, previous)) {
		t.Fatal("an epoch from another boot was not judged as a previous boot")
	}
	if judge.DeliveryEpochFromPreviousBoot(fmt.Sprintf("host-4242-%d-%s", started, current)) {
		t.Fatal("an epoch from this boot was judged as a previous boot")
	}
}
