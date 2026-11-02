package persistenthost

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestNativeUnknownRPCErrorCannotSettleUnreservedPrompt(t *testing.T) {
	j, err := newDeliveryJournal(filepath.Join(t.TempDir(), "journal"), "epoch")
	if err != nil {
		t.Fatal(err)
	}
	defer j.file.Close()
	if _, err = j.prepare([]byte(`{"id":7,"method":"session/prompt","params":{"sessionId":"native"}}`)); err != nil {
		t.Fatal(err)
	}
	if _, err = j.query(nil, true); !errors.Is(err, ports.ErrChatShutdownNotSettled) {
		t.Fatalf("setup gap not tracked: %v", err)
	}
	if err = j.observe([]byte(`{"id":7,"error":{"code":-32603,"message":"outcome unknown"}}`)); err != nil {
		t.Fatal(err)
	}
	if _, err = j.query(nil, true); err == nil {
		t.Fatal("arbitrary RPC error was treated as settled without stopReason/not-forwarded proof")
	}
}

func TestNativeCompactAckBeforeNativeTurnCannotSettle(t *testing.T) {
	j, err := newDeliveryJournal(filepath.Join(t.TempDir(), "journal"), "epoch")
	if err != nil {
		t.Fatal(err)
	}
	defer j.file.Close()
	if _, err = j.prepare([]byte(`{"id":8,"method":"thread/compact/start","params":{"threadId":"native"}}`)); err != nil {
		t.Fatal(err)
	}
	if err = j.observe([]byte(`{"id":8,"result":{}}`)); err != nil {
		t.Fatal(err)
	}
	if _, err = j.query(nil, true); err == nil {
		t.Fatal("compact ACK was treated as terminal before native turn/started/completed")
	}
}

func TestNativeACPProgressDoesNotRefreshPreviousUnknownAttempt(t *testing.T) {
	j, err := newDeliveryJournal(filepath.Join(t.TempDir(), "journal"), "epoch")
	if err != nil {
		t.Fatal(err)
	}
	defer j.file.Close()
	if _, err = j.prepare([]byte(`{"id":1,"method":"session/prompt","params":{"sessionId":"same-native","_meta":{"aoDeliveryAttempt":"old-unknown"}}}`)); err != nil {
		t.Fatal(err)
	}
	if err = j.observe([]byte(`{"id":1,"error":{"code":-32603,"message":"unknown outcome"}}`)); err != nil {
		t.Fatal(err)
	}
	old, err := j.query([]ports.ChatDeliveryLookup{{AttemptID: "old-unknown"}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = j.prepare([]byte(`{"id":2,"method":"session/prompt","params":{"sessionId":"same-native","_meta":{"aoDeliveryAttempt":"new-current"}}}`)); err != nil {
		t.Fatal(err)
	}
	if err = j.observe([]byte(`{"method":"session/update","params":{"sessionId":"same-native","update":{"sessionUpdate":"agent_message_chunk"}}}`)); err != nil {
		t.Fatal(err)
	}
	refreshed, err := j.query([]ports.ChatDeliveryLookup{{AttemptID: "old-unknown"}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed[0].ProgressSequence != old[0].ProgressSequence {
		t.Fatalf("new prompt progress refreshed prior unknown attempt: seq %d -> %d", old[0].ProgressSequence, refreshed[0].ProgressSequence)
	}
}
