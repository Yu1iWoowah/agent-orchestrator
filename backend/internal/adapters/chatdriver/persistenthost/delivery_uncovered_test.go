package persistenthost

import (
	"path/filepath"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestManagedNativeCustodyAllowsInFlightSteerFlush(t *testing.T) {
	j, err := newDeliveryJournal(filepath.Join(t.TempDir(), "journal"), "epoch")
	if err != nil {
		t.Fatal(err)
	}
	defer j.file.Close()
	for _, frame := range []string{
		`{"id":1,"method":"turn/start","params":{"threadId":"native","_meta":{"aoDeliveryAttempt":"managed"}}}`,
		`{"id":2,"method":"turn/steer","params":{"threadId":"native","expectedTurnId":"turn"}}`,
	} {
		if _, err = j.prepare([]byte(frame)); err != nil {
			t.Fatal(err)
		}
	}
	for _, frame := range []string{`{"id":1,"result":{"turn":{"id":"turn"}}}`, `{"id":2,"result":{"turnId":"turn"}}`} {
		if err = j.observe([]byte(frame)); err != nil {
			t.Fatal(err)
		}
	}
	receipts, err := j.query([]ports.ChatDeliveryLookup{{AttemptID: "managed"}}, true)
	if err != nil || len(receipts) != 1 || !receipts[0].CustodyFlushed || receipts[0].ExecutionSettled {
		t.Fatalf("native L2 custody required completion: %+v %v", receipts, err)
	}
}

func TestExplicitUnsupportedControlDoesNotPoisonFallback(t *testing.T) {
	for _, reply := range []string{`{"id":1,"error":{"code":-32601,"message":"not found"}}`, `{"id":1,"error":{"code":-32600,"message":"unknown variant \u0060thread/rollback\u0060"}}`} {
		j, err := newDeliveryJournal(filepath.Join(t.TempDir(), "journal"), "epoch")
		if err != nil {
			t.Fatal(err)
		}
		defer j.file.Close()
		if _, err = j.prepare([]byte(`{"id":1,"method":"thread/rollback","params":{"threadId":"native"}}`)); err != nil {
			t.Fatal(err)
		}
		if err = j.observe([]byte(reply)); err != nil {
			t.Fatal(err)
		}
		if _, err = j.query(nil, true); err != nil {
			t.Fatalf("explicit unsupported method prevented fallback: %v", err)
		}
	}
}
func TestCompactNativeEventsBeforeAckStillConverge(t *testing.T) {
	j, err := newDeliveryJournal(filepath.Join(t.TempDir(), "journal"), "epoch")
	if err != nil {
		t.Fatal(err)
	}
	defer j.file.Close()
	if _, err = j.prepare([]byte(`{"id":1,"method":"thread/compact/start","params":{"threadId":"native"}}`)); err != nil {
		t.Fatal(err)
	}
	for _, frame := range []string{`{"method":"item/started","params":{"threadId":"native","turnId":"original","item":{"type":"contextCompaction","id":"compact"}}}`, `{"method":"turn/completed","params":{"threadId":"native","turn":{"id":"original"}}}`, `{"id":1,"result":{}}`} {
		if err = j.observe([]byte(frame)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = j.query(nil, true); err != nil {
		t.Fatalf("terminal before ACK remained blocked: %v", err)
	}
}

func TestSteerTerminalBeforeReplyStillConverges(t *testing.T) {
	j, err := newDeliveryJournal(filepath.Join(t.TempDir(), "journal"), "epoch")
	if err != nil {
		t.Fatal(err)
	}
	defer j.file.Close()
	if _, err = j.prepare([]byte(`{"id":1,"method":"turn/steer","params":{"threadId":"native","expectedTurnId":"turn"}}`)); err != nil {
		t.Fatal(err)
	}
	if err = j.observe([]byte(`{"method":"turn/completed","params":{"threadId":"native","turn":{"id":"turn"}}}`)); err != nil {
		t.Fatal(err)
	}
	if _, err = j.query(nil, true); err == nil {
		t.Fatal("terminal alone qualified unanswered steer")
	}
	if err = j.observe([]byte(`{"id":1,"result":{"turnId":"turn"}}`)); err != nil {
		t.Fatal(err)
	}
	if _, err = j.query(nil, true); err != nil {
		t.Fatalf("matched terminal/response order retained gap: %v", err)
	}
}
