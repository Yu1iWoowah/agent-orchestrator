package persistenthost

import (
	"errors"
	"path/filepath"
	"strings"
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

// ACP _session/steering settles only on the extension's own evidence: a
// refusal immediately, injected guidance once the prompts it could join end.
func TestACPSteeringSettlesOnItsAnswer(t *testing.T) {
	const (
		managedPrompt   = `{"id":1,"method":"session/prompt","params":{"sessionId":"s1","prompt":[],"_meta":{"aoDeliveryAttempt":"managed"}}}`
		secondPrompt    = `{"id":3,"method":"session/prompt","params":{"sessionId":"s1","prompt":[],"_meta":{"aoDeliveryAttempt":"managed2"}}}`
		unmanagedPrompt = `{"id":4,"method":"session/prompt","params":{"sessionId":"s1","prompt":[{"type":"text","text":"/compact"}]}}`
		steer           = `{"id":2,"method":"_session/steering","params":{"sessionId":"s1","prompt":[],"_meta":{"steering":{"idleBehavior":"promptRequired"}}}}`
		managedEnd      = `{"id":1,"result":{"stopReason":"end_turn"}}`
		secondEnd       = `{"id":3,"result":{"stopReason":"end_turn"}}`
		unmanagedEnd    = `{"id":4,"result":{"stopReason":"end_turn"}}`
	)
	steerAnswer := func(outcome string) string { return `{"id":2,"result":{"outcome":"` + outcome + `"}}` }
	for _, tc := range []struct {
		name    string
		frames  []string // "> " prefix: client frame (prepare); otherwise provider frame (observe)
		settled bool
	}{
		{"injected then prompt ends", []string{"> " + managedPrompt, "> " + steer, steerAnswer("injected"), managedEnd}, true},
		{"prompt ends before the injected answer", []string{"> " + managedPrompt, "> " + steer, managedEnd, steerAnswer("injected")}, true},
		{"injected waits for every prompt", []string{"> " + managedPrompt, "> " + secondPrompt, "> " + steer, steerAnswer("injected"), managedEnd}, false},
		{"injected after every prompt ends", []string{"> " + managedPrompt, "> " + secondPrompt, "> " + steer, steerAnswer("injected"), managedEnd, secondEnd}, true},
		{"injected into an unmanaged prompt", []string{"> " + unmanagedPrompt, "> " + steer, steerAnswer("injected"), unmanagedEnd}, true},
		{"rpc error joins the prompt", []string{"> " + managedPrompt, "> " + steer, `{"id":2,"error":{"code":-32603,"message":"internal"}}`, managedEnd}, true},
		{"promptRequired settles at once", []string{"> " + steer, steerAnswer("promptRequired")}, true},
		{"failed settles at once", []string{"> " + managedPrompt, "> " + steer, steerAnswer("failed")}, true},
		{"startedNewTurn stays", []string{"> " + managedPrompt, "> " + steer, steerAnswer("startedNewTurn"), managedEnd}, false},
		{"unknown outcome stays", []string{"> " + managedPrompt, "> " + steer, steerAnswer("queued"), managedEnd}, false},
		{"injected with no prompt in flight stays", []string{"> " + steer, steerAnswer("injected")}, false},
		{"unanswered steer stays", []string{"> " + managedPrompt, "> " + steer, managedEnd}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j, err := newDeliveryJournal(filepath.Join(t.TempDir(), "journal"), "epoch")
			if err != nil {
				t.Fatal(err)
			}
			defer j.file.Close()
			for _, frame := range tc.frames {
				if strings.HasPrefix(frame, "> ") {
					_, err = j.prepare([]byte(strings.TrimPrefix(frame, "> ")))
				} else {
					err = j.observe([]byte(frame))
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			_, err = j.query(nil, true)
			if tc.settled && err != nil {
				t.Fatalf("flush refused after a settled steer: %v (gaps %v)", err, j.unsettled)
			}
			if !tc.settled && !errors.Is(err, ports.ErrChatShutdownNotSettled) {
				t.Fatalf("flush settled an unproven steer: %v", err)
			}
		})
	}
}
