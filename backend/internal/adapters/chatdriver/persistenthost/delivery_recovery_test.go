package persistenthost

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func recoveryJournal(t *testing.T) *deliveryJournal {
	t.Helper()
	j, err := newDeliveryJournal(filepath.Join(t.TempDir(), "delivery.journal"), "epoch")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = j.file.Close() })
	return j
}
func deliveryFrame(t *testing.T, j *deliveryJournal, frame string, request bool) {
	t.Helper()
	var err error
	if request {
		_, err = j.prepare([]byte(frame))
	} else {
		err = j.observe([]byte(frame))
	}
	if err != nil {
		t.Fatal(err)
	}
}
func assertDeliveryFlush(t *testing.T, j *deliveryJournal, settled bool) {
	t.Helper()
	_, err := j.query(nil, true)
	if settled && err != nil || !settled && !errors.Is(err, ports.ErrChatShutdownNotSettled) {
		t.Fatalf("settled=%v: flush=%v", settled, err)
	}
}
func TestDeliveryControlResponsesReleaseCoverage(t *testing.T) {
	for _, method := range []string{"config/mcpServer/reload", "thread/delete"} {
		for _, response := range []string{`"result":{}`, `"result":null`, `"error":{"code":-32602,"message":"rejected"}`} {
			t.Run(method+response, func(t *testing.T) {
				j := recoveryJournal(t)
				deliveryFrame(t, j, fmt.Sprintf(`{"id":1,"method":%q,"params":{}}`, method), true)
				assertDeliveryFlush(t, j, false)
				deliveryFrame(t, j, `{"id":1,`+response+`}`, false)
				assertDeliveryFlush(t, j, true)
			})
		}
	}
}
func TestDeliverySteerRefusalAndTerminalReleaseCoverage(t *testing.T) {
	for _, reason := range []string{"no active turn to steer", "cannot steer a compact turn", "unknown-before-terminal", "terminal-before-unknown"} {
		t.Run(reason, func(t *testing.T) {
			j := recoveryJournal(t)
			deliveryFrame(t, j, `{"id":1,"method":"turn/steer","params":{"threadId":"thread","expectedTurnId":"target"}}`, true)
			terminal := `{"method":"turn/completed","params":{"threadId":"thread","turn":{"id":"target"}}}`
			if reason == "terminal-before-unknown" {
				deliveryFrame(t, j, terminal, false)
			}
			code := -32600
			if reason == "unknown-before-terminal" || reason == "terminal-before-unknown" {
				code = -32603
			}
			deliveryFrame(t, j, fmt.Sprintf(`{"id":1,"error":{"code":%d,"message":%q}}`, code, reason), false)
			if reason == "unknown-before-terminal" {
				assertDeliveryFlush(t, j, false)
				deliveryFrame(t, j, terminal, false)
			}
			assertDeliveryFlush(t, j, true)
		})
	}
}
func TestDeliveryFreshNativeReadReconcilesOnlyCoveredGaps(t *testing.T) {
	for _, method := range []string{"turn/steer", "thread/compact/start", "thread/rollback"} {
		for _, proof := range []string{"idle", "active", "other-thread", "missing-turns", "null-turns", "failed-read", "stale-read"} {
			t.Run(method+"/"+proof, func(t *testing.T) {
				j := recoveryJournal(t)
				deliveryFrame(t, j, fmt.Sprintf(`{"id":1,"method":%q,"params":{"threadId":"thread","expectedTurnId":"target"}}`, method), true)
				read := `{"id":2,"method":"thread/read","params":{"threadId":"thread","includeTurns":true}}`
				if proof == "stale-read" {
					deliveryFrame(t, j, read, true)
				}
				response := `{"id":1,"error":{"code":-32603,"message":"indeterminate"}}`
				if method == "thread/compact/start" {
					response = `{"id":1,"result":{}}`
				}
				deliveryFrame(t, j, response, false)
				assertDeliveryFlush(t, j, false)
				if proof != "stale-read" {
					deliveryFrame(t, j, read, true)
				}
				response = `{"id":2,"result":{"thread":{"id":"thread","status":{"type":"idle"},"turns":[]}}}`
				switch proof {
				case "active":
					response = `{"id":2,"result":{"thread":{"id":"thread","status":{"type":"active"},"turns":[{"id":"target","status":"inProgress"}]}}}`
				case "other-thread":
					response = `{"id":2,"result":{"thread":{"id":"other","status":{"type":"idle"},"turns":[]}}}`
				case "missing-turns":
					response = `{"id":2,"result":{"thread":{"id":"thread","status":{"type":"idle"}}}}`
				case "null-turns":
					response = `{"id":2,"result":{"thread":{"id":"thread","status":{"type":"idle"},"turns":null}}}`
				case "failed-read":
					response = `{"id":2,"error":{"code":-32603,"message":"unavailable"}}`
				}
				deliveryFrame(t, j, response, false)
				assertDeliveryFlush(t, j, proof == "idle" && method != "thread/compact/start")
				if proof != "idle" {
					deliveryFrame(t, j, `{"id":3,"method":"thread/read","params":{"threadId":"thread","includeTurns":true}}`, true)
					deliveryFrame(t, j, `{"id":3,"result":{"thread":{"id":"thread","status":{"type":"idle"},"turns":[]}}}`, false)
					assertDeliveryFlush(t, j, method != "thread/compact/start")
				}
				if method == "thread/compact/start" {
					deliveryFrame(t, j, `{"method":"item/started","params":{"threadId":"thread","turnId":"compact","item":{"type":"contextCompaction","id":"compact-item"}}}`, false)
					deliveryFrame(t, j, `{"method":"turn/completed","params":{"threadId":"thread","turn":{"id":"compact"}}}`, false)
					assertDeliveryFlush(t, j, true)
				}
			})
		}
	}
}
func TestDeliveryNativeReadCannotReleaseLaterOperation(t *testing.T) {
	j := recoveryJournal(t)
	deliveryFrame(t, j, `{"id":1,"method":"thread/compact/start","params":{"threadId":"thread"}}`, true)
	deliveryFrame(t, j, `{"id":1,"result":{}}`, false)
	deliveryFrame(t, j, `{"id":2,"method":"thread/read","params":{"threadId":"thread","includeTurns":true}}`, true)
	deliveryFrame(t, j, `{"id":3,"method":"thread/compact/start","params":{"threadId":"thread"}}`, true)
	deliveryFrame(t, j, `{"id":3,"result":{}}`, false)
	deliveryFrame(t, j, `{"id":2,"result":{"thread":{"id":"thread","status":{"type":"idle"},"turns":[]}}}`, false)
	assertDeliveryFlush(t, j, false)
}

func TestDeliveryIdleReadCannotEraseExecutionStartedAfterQuery(t *testing.T) {
	j := recoveryJournal(t)
	deliveryFrame(t, j, `{"id":1,"method":"thread/compact/start","params":{"threadId":"thread"}}`, true)
	deliveryFrame(t, j, `{"id":1,"result":{}}`, false)
	deliveryFrame(t, j, `{"id":2,"method":"thread/read","params":{"threadId":"thread","includeTurns":true}}`, true)
	deliveryFrame(t, j, `{"method":"turn/started","params":{"threadId":"thread","turn":{"id":"late-compact"}}}`, false)
	deliveryFrame(t, j, `{"method":"item/started","params":{"threadId":"thread","turnId":"late-compact","item":{"type":"contextCompaction","id":"compact"}}}`, false)
	deliveryFrame(t, j, `{"id":2,"result":{"thread":{"id":"thread","status":{"type":"idle"},"turns":[]}}}`, false)
	assertDeliveryFlush(t, j, false)
	deliveryFrame(t, j, `{"method":"turn/completed","params":{"threadId":"thread","turn":{"id":"late-compact"}}}`, false)
	assertDeliveryFlush(t, j, true)
}

func TestDeliveryIdleCannotProveUnknownExecutionStartDidNotQueue(t *testing.T) {
	for _, method := range []string{"turn/start", "future/native/start"} {
		t.Run(method, func(t *testing.T) {
			j := recoveryJournal(t)
			deliveryFrame(t, j, fmt.Sprintf(`{"id":1,"method":%q,"params":{"threadId":"thread"}}`, method), true)
			deliveryFrame(t, j, `{"id":1,"error":{"code":-32603,"message":"indeterminate"}}`, false)
			deliveryFrame(t, j, `{"id":2,"method":"thread/read","params":{"threadId":"thread","includeTurns":true}}`, true)
			deliveryFrame(t, j, `{"id":2,"result":{"thread":{"id":"thread","status":{"type":"idle"},"turns":[]}}}`, false)
			assertDeliveryFlush(t, j, false)
		})
	}
}

func TestDeliveryIdleReadReconcilesKnownNativeExecution(t *testing.T) {
	j := recoveryJournal(t)
	deliveryFrame(t, j, `{"method":"turn/started","params":{"threadId":"thread","turn":{"id":"background"}}}`, false)
	assertDeliveryFlush(t, j, false)
	deliveryFrame(t, j, `{"id":1,"method":"thread/read","params":{"threadId":"thread","includeTurns":true}}`, true)
	deliveryFrame(t, j, `{"method":"turn/started","params":{"threadId":"thread","turn":{"id":"later"}}}`, false)
	deliveryFrame(t, j, `{"id":1,"result":{"thread":{"id":"thread","status":{"type":"idle"},"turns":[]}}}`, false)
	assertDeliveryFlush(t, j, false)
	deliveryFrame(t, j, `{"method":"turn/completed","params":{"threadId":"thread","turn":{"id":"later"}}}`, false)
	assertDeliveryFlush(t, j, true)
}
