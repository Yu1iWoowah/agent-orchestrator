package chat

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type blockedCompactionCall struct {
	*recordCompactionConversation
	entered chan struct{}
	release chan struct{}
}

func (c *blockedCompactionCall) Compact(ctx context.Context) (ports.ChatCompactionResult, error) {
	close(c.entered)
	select {
	case <-c.release:
	case <-ctx.Done():
		return ports.ChatCompactionResult{}, ctx.Err()
	}
	return c.recordCompactionConversation.Compact(ctx)
}

func TestSideCompactionDispatchVetoesShutdownBeforeNativeIO(t *testing.T) {
	for _, reply := range []string{"success", "unknown"} {
		t.Run(reply, func(t *testing.T) {
			svc, db, conv, _ := newRecordHarness(t, domain.HarnessCodex)
			if reply == "success" {
				conv.compactErr = nil
			}
			conv.idle.Store(true)
			gate := &blockedCompactionCall{recordCompactionConversation: conv, entered: make(chan struct{}), release: make(chan struct{})}
			runtime := svc.sides.runtimes["side"]
			runtime.conv = gate
			done := make(chan struct{})
			go func() { defer close(done); _, _ = svc.CompactSideChat(t.Context(), "session", "side") }()
			released := false
			defer func() {
				if !released {
					close(gate.release)
					<-done
				}
			}()
			select {
			case <-gate.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("Compact did not reach the dispatch gate")
			}
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			if svc.sides.reconcileCompaction(ctx, runtime, true) {
				t.Fatal("force bypassed the dispatch fence")
			}
			err := svc.PreflightShutdown(ctx)
			rows, readErr := db.SideCompactions(t.Context(), false)
			store := svc.sides.store.(*memorySideStore)
			store.mu.Lock()
			protected := store.protected["side"]
			store.mu.Unlock()
			if !errors.Is(err, ports.ErrChatShutdownNotSettled) || readErr != nil || len(rows) != 1 || rows[0].State != domain.SideCompactionPending || len(rows[0].Content) == 0 || !protected {
				t.Fatalf("preflight=%v records=%+v protected=%v read=%v; want pending protection and shutdown veto before native I/O", err, rows, protected, readErr)
			}
			if conv.count != 0 || conv.queries.Load() != 0 {
				t.Fatalf("pre-dispatch provider calls: compact=%d reconcile=%d", conv.count, conv.queries.Load())
			}
			close(gate.release)
			released = true
			<-done
			if err := svc.PreflightShutdown(t.Context()); err != nil {
				t.Fatalf("completed Compact call retained its dispatch fence: %v", err)
			}
			rows, readErr = db.SideCompactions(t.Context(), false)
			if readErr != nil || len(rows) != 1 || rows[0].State != domain.SideCompactionSettled {
				t.Fatalf("original call did not become settleable after return: %v, %v", rows, readErr)
			}
		})
	}
}

func TestSideCompactionOriginalReceiptCanSettleWithoutReplay(t *testing.T) {
	for _, via := range []string{"preflight", "compact_retry"} {
		for _, outcome := range []ports.ChatCompactionOutcome{ports.ChatCompactionSettled, ports.ChatCompactionNotSent} {
			t.Run(via+"/"+string(outcome), func(t *testing.T) {
				svc, db, conv, _ := newRecordHarness(t, domain.HarnessCodex)
				_, _ = svc.CompactSideChat(t.Context(), "session", "side")
				writer := recordWriter(t, svc)
				if _, err := writer.Exec(`CREATE TRIGGER reject_compaction_result BEFORE UPDATE OF state ON chat_side_compactions BEGIN SELECT RAISE(ABORT,'temporary result failure'); END`); err != nil {
					t.Fatal(err)
				}
				log, lines := reviewLogger()
				svc.log = log
				conv.events, conv.acknowledged = make(chan ports.ChatEvent, 1), make(chan string, 2)
				runtime := svc.sides.runtimes["side"]
				id := runtime.compactionID
				ctx, cancel := context.WithCancel(t.Context())
				done := make(chan struct{})
				go func() { defer close(done); svc.sides.consumeEvents(ctx, runtime) }()
				defer func() { cancel(); <-done }()
				conv.events <- ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "native-compact", ProviderEventID: "original-receipt", CompactionID: id, Compaction: &ports.ChatCompactionReceipt{CommandID: id, Outcome: outcome}}
				requireReviewWarning(t, lines, "CHAT_SIDE_COMPACTION_RESULT_FAILED")
				select {
				case <-conv.acknowledged:
					t.Fatal("receipt acked before durable settlement")
				default:
				}
				if _, err := writer.Exec(`DROP TRIGGER reject_compaction_result`); err != nil {
					t.Fatal(err)
				}
				if via == "preflight" {
					if err := svc.PreflightShutdown(t.Context()); err != nil {
						t.Fatalf("original receipt could not settle after SQLite recovery: %v", err)
					}
				} else {
					if _, err := svc.CompactSideChat(t.Context(), "session", "side"); !errors.Is(err, ErrSideCompactionReconciled) {
						t.Fatalf("retry did not settle the original receipt: %v", err)
					}
				}
				select {
				case ack := <-conv.acknowledged:
					if ack != "original-receipt" {
						t.Fatal(ack)
					}
				default:
					t.Fatal("durably settled receipt was not acked")
				}
				select {
				case <-conv.acknowledged:
					t.Fatal("receipt acked twice")
				default:
				}
				rows, err := db.SideCompactions(t.Context(), false)
				state := domain.SideCompactionSettled
				if outcome == ports.ChatCompactionNotSent {
					state = domain.SideCompactionNotSent
				}
				if err != nil || len(rows) != 1 || rows[0].State != state || len(rows[0].Content) != 0 {
					t.Fatalf("original result not settled: %+v, %v", rows, err)
				}
				if conv.count != 1 || conv.queries.Load() != 0 {
					t.Fatalf("replayed compaction or substituted native inference: io=%d query=%d", conv.count, conv.queries.Load())
				}
			})
		}
	}
}
