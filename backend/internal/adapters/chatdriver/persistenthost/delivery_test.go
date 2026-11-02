package persistenthost

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestDeliveryJournalRequiresMatchedNativeResponse(t *testing.T) {
	j, err := newDeliveryJournal(filepath.Join(t.TempDir(), "delivery.journal"), "native-epoch")
	if err != nil {
		t.Fatal(err)
	}
	defer j.file.Close()
	frame, err := j.prepare([]byte(`{"id":12,"method":"turn/start","params":{"_meta":{"aoDeliveryAttempt":"attempt-1"},"input":[]}}`))
	if err != nil {
		t.Fatal(err)
	}
	// The private correlation key must not reach native provider code.
	if string(frame) == "" {
		t.Fatal("native frame missing")
	}
	lookup := []ports.ChatDeliveryLookup{{AttemptID: "attempt-1"}}
	initial, err := j.query(lookup, true)
	if err != nil || initial[0].Disposition != domain.ChatDeliveryUnknown {
		t.Fatalf("write promoted to L2: %+v %v", initial, err)
	}
	if err = j.observe([]byte(`{"id":99,"result":{"turn":{"id":"unrelated"}}}`)); err != nil {
		t.Fatal(err)
	}
	if _, err = j.prepare([]byte(`{"id":11,"method":"turn/start","params":{"_meta":{"aoDeliveryAttempt":"malformed-response"}}}`)); err != nil {
		t.Fatal(err)
	}
	if err = j.observe([]byte(`{"id":11,"result":{}}`)); err != nil {
		t.Fatal(err)
	}
	unmatched, _ := j.query([]ports.ChatDeliveryLookup{{AttemptID: "malformed-response"}}, false)
	if unmatched[0].Disposition != domain.ChatDeliveryUnknown {
		t.Fatal("missing native id accepted")
	}
	if err = j.observe([]byte(`{"id":12,"result":{"turn":{"id":"native-turn"}}}`)); err != nil {
		t.Fatal(err)
	}
	accepted, err := j.query(lookup, true)
	if err != nil || accepted[0].Disposition != domain.ChatDeliveryAccepted || !accepted[0].CustodyFlushed || accepted[0].ProviderRequestID != "12" {
		t.Fatalf("native response: %+v %v", accepted, err)
	}
	if _, err = j.query([]ports.ChatDeliveryLookup{{AttemptID: "missing"}}, false); !errors.Is(err, ports.ErrChatDeliveryUnverified) {
		t.Fatal("absence claimed not-sent")
	}
	if _, err = j.prepare([]byte(`{"id":13,"method":"turn/steer","params":{}}`)); err != nil {
		t.Fatal(err)
	}
	if _, err = j.query(lookup, true); !errors.Is(err, ports.ErrChatShutdownNotSettled) {
		t.Fatal("unmanaged outbound silently omitted")
	}
}

func TestHostDeliveryQuerySurvivesDaemonAttachment(t *testing.T) {
	cfg := Config{SessionID: "delivery-reconnect", DataDir: t.TempDir(), Workdir: t.TempDir(), Env: append(os.Environ(), "AO_CHAT_HOST_PROVIDER_HELPER=1"), Argv: []string{os.Args[0], "-test.run=TestProviderHelper"}}
	done := make(chan error, 1)
	go func() { done <- Run(context.Background(), cfg) }()
	descriptor := awaitDescriptor(t, cfg.DataDir, cfg.SessionID)
	t.Cleanup(func() { _ = Shutdown(context.Background(), cfg.DataDir, cfg.SessionID); <-done })
	first := awaitAttach(t, descriptor)
	sendFrame(t, first, `{"id":1,"method":"turn/start","params":{"_meta":{"aoDeliveryAttempt":"surviving-attempt"},"input":[]}}`)
	_ = readFrame(t, bufio.NewReader(first.Stdout))
	before, err := first.Delivery.QueryDeliveryAttempt(t.Context(), ports.ChatDeliveryLookup{AttemptID: "surviving-attempt"})
	if err != nil || before.Disposition != domain.ChatDeliveryAccepted {
		t.Fatalf("receipt: %+v %v", before, err)
	}
	_ = first.Stdin.Close()
	next := awaitAttach(t, descriptor)
	defer next.Stdin.Close()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	receipts, err := next.Delivery.FenceAndFlushOutbound(ctx, []ports.ChatDeliveryLookup{{AttemptID: before.AttemptID, ProviderEpoch: before.ProviderEpoch, ProviderRequestID: before.ProviderRequestID}})
	if err != nil || len(receipts) != 1 || !receipts[0].CustodyFlushed || receipts[0].ProviderTurnID != before.ProviderTurnID {
		t.Fatalf("reattach lost original receipt: %+v %v", receipts, err)
	}
}

func TestDeliveryFlushRequiresHealthyReplayJournals(t *testing.T) {
	relay, err := newACPRelay(t.Context(), filepath.Join(t.TempDir(), "prompt.journal"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = relay.close(context.Background()) })
	h := &host{acp: relay}
	if err = h.flushReplay(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = relay.promptJournal.file.Close(); err != nil {
		t.Fatal(err)
	}
	if err = h.flushReplay(t.Context()); err == nil {
		t.Fatal("closed replay journal reported flushed custody")
	}
}

func TestACPDeliveryTracksNativeProgressUntilPromptResponse(t *testing.T) {
	j, err := newDeliveryJournal(filepath.Join(t.TempDir(), "delivery.journal"), "native-epoch")
	if err != nil {
		t.Fatal(err)
	}
	defer j.file.Close()
	_, err = j.prepare([]byte(`{"id":12,"method":"session/prompt","params":{"sessionId":"native-session","_meta":{"aoDeliveryAttempt":"acp-attempt"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	key := []ports.ChatDeliveryLookup{{AttemptID: "acp-attempt"}}
	before, _ := j.query(key, false)
	if err := j.observe([]byte(`{"method":"session/update","params":{"sessionId":"other-session","update":{"sessionUpdate":"agent_message_chunk"}}}`)); err != nil {
		t.Fatal(err)
	}
	unrelated, _ := j.query(key, false)
	if !unrelated[0].ObservedAt.Equal(before[0].ObservedAt) {
		t.Fatal("unrelated session advanced the attempt")
	}
	for _, tc := range []struct{ frame, progress string }{
		{`{"method":"session/update","params":{"sessionId":"native-session","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"private prose"}}}}`, "running"},
		{`{"id":"approval-1","method":"session/request_permission","params":{"sessionId":"native-session"}}`, "approval"},
		{`{"method":"session/update","params":{"sessionId":"native-session","update":{"sessionUpdate":"tool_call","toolCallId":"tool-1","status":"in_progress"}}}`, "tool"},
	} {
		if err := j.observe([]byte(tc.frame)); err != nil {
			t.Fatal(err)
		}
		got, err := j.query(key, false)
		if err != nil || got[0].Progress != tc.progress || !got[0].ObservedAt.After(before[0].ObservedAt) {
			t.Fatalf("ACP native progress missing: %+v %v", got, err)
		}
		if tc.progress == "approval" {
			_, err = j.prepare([]byte(`{"id":"approval-1","result":{"outcome":{"outcome":"selected","optionId":"allow"}}}`))
			if err != nil {
				t.Fatal(err)
			}
		}
		if got[0].Disposition != domain.ChatDeliveryUnknown || got[0].Complete || got[0].ExecutionSettled {
			t.Fatalf("progress fabricated acceptance: %+v", got[0])
		}
	}
	if err := j.observe([]byte(`{"id":12,"result":{"stopReason":"end_turn"}}`)); err != nil {
		t.Fatal(err)
	}
	done, err := j.query(key, false)
	if err != nil || !done[0].ExecutionSettled || done[0].Disposition != domain.ChatDeliveryAccepted {
		t.Fatalf("ACP completion did not settle original attempt: %+v %v", done, err)
	}
	data, _ := os.ReadFile(j.file.Name())
	if strings.Contains(string(data), "private prose") {
		t.Fatal("progress journal retained provider prose")
	}
}

func TestUncoveredRequestRequiresMatchingSettlement(t *testing.T) {
	for _, method := range []string{"turn/steer", "thread/rollback", "thread/revert", "thread/fork", "thread/compact/start", "session/prompt", "turn/start"} {
		for _, failure := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/error=%v", method, failure), func(t *testing.T) {
				j, err := newDeliveryJournal(filepath.Join(t.TempDir(), "delivery.journal"), "epoch")
				if err != nil {
					t.Fatal(err)
				}
				defer j.file.Close()
				if _, err = j.prepare([]byte(fmt.Sprintf(`{"id":7,"method":%q,"params":{"threadId":"native","expectedTurnId":"native-turn"}}`, method))); err != nil {
					t.Fatal(err)
				}
				if _, err = j.query(nil, true); !errors.Is(err, ports.ErrChatShutdownNotSettled) {
					t.Fatalf("in-flight request omitted: %v", err)
				}
				response := `{"id":7,"result":{}}`
				if method == "turn/steer" {
					response = `{"id":7,"result":{"turnId":"native-turn"}}`
				}
				if method == "session/prompt" {
					response = `{"id":7,"result":{"stopReason":"end_turn"}}`
				}
				if method == "turn/start" {
					response = `{"id":7,"result":{"turn":{"id":"native-turn"}}}`
				}
				if failure {
					response = `{"id":7,"error":{"code":-32602,"message":"refused"}}`
				}
				if err = j.observe([]byte(response)); err != nil {
					t.Fatal(err)
				}
				if failure {
					if _, err = j.query(nil, true); !errors.Is(err, ports.ErrChatShutdownNotSettled) {
						t.Fatalf("RPC error erased unknown execution: %v", err)
					}
					return
				}
				if method == "thread/compact/start" || method == "turn/steer" {
					if _, err = j.query(nil, true); !errors.Is(err, ports.ErrChatShutdownNotSettled) {
						t.Fatalf("async ACK erased uncovered execution: %v", err)
					}
					if method == "thread/compact/start" {
						for _, frame := range []string{
							`{"method":"turn/started","params":{"threadId":"native","turn":{"id":"native-turn"}}}`,
							`{"method":"item/started","params":{"threadId":"native","turnId":"native-turn","item":{"type":"contextCompaction","id":"compact"}}}`,
						} {
							if err = j.observe([]byte(frame)); err != nil {
								t.Fatal(err)
							}
						}
						if err = j.observe([]byte(`{"method":"turn/completed","params":{"threadId":"native","turn":{"id":"unrelated-turn"}}}`)); err != nil {
							t.Fatal(err)
						}
						if _, err = j.query(nil, true); err == nil {
							t.Fatal("unrelated terminal erased compact gap")
						}
					}
					if err = j.observe([]byte(`{"method":"turn/completed","params":{"threadId":"native","turn":{"id":"native-turn"}}}`)); err != nil {
						t.Fatal(err)
					}
				}
				if method == "turn/start" && !failure {
					if _, err = j.query(nil, true); !errors.Is(err, ports.ErrChatShutdownNotSettled) {
						t.Fatalf("native execution omitted: %v", err)
					}
					if err := j.observe([]byte(`{"id":99,"method":"item/commandExecution/requestApproval","params":{"turnId":"native-turn"}}`)); err != nil {
						t.Fatal(err)
					}
					if err = j.observe([]byte(`{"method":"turn/completed","params":{"threadId":"native","turn":{"id":"native-turn"}}}`)); err != nil {
						t.Fatal(err)
					}
					if len(j.waiting) != 0 {
						t.Fatal("completed unreserved turn retained native waits")
					}
				}
				if _, err = j.query(nil, true); err != nil {
					t.Fatalf("completed request permanently blocked flush: %v", err)
				}
			})
		}
	}
}

func TestDeliveryAppendFailureDoesNotStopProviderForwarding(t *testing.T) {
	j, err := newDeliveryJournal(filepath.Join(t.TempDir(), "delivery.journal"), "epoch")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = j.prepare([]byte(`{"id":1,"method":"turn/start","params":{"_meta":{"aoDeliveryAttempt":"attempt"}}}`)); err != nil {
		t.Fatal(err)
	}
	if err = j.file.Close(); err != nil {
		t.Fatal(err)
	}
	h := &host{ctx: t.Context(), delivery: j, pendingRequests: map[string]*pendingRequest{}}
	frames := `{"id":1,"result":{"turn":{"id":"native"}}}` + "\n" + `{"method":"turn/completed","params":{"turn":{"id":"native"}}}` + "\n"
	if err = h.forwardProvider(strings.NewReader(frames)); !errors.Is(err, io.EOF) {
		t.Fatalf("evidence failure interrupted provider forwarding: %v", err)
	}
	if len(h.detached) != 2 {
		t.Fatalf("forwarded %d frames, want 2", len(h.detached))
	}
	if got, err := j.query([]ports.ChatDeliveryLookup{{AttemptID: "attempt"}}, false); err != nil || !got[0].ExecutionSettled {
		t.Fatalf("memory receipt lost: %+v %v", got, err)
	}
	frame, err := j.prepare([]byte(`{"id":2,"method":"turn/start","params":{"_meta":{"aoDeliveryAttempt":"later"}}}`))
	if len(frame) == 0 || strings.Contains(string(frame), DeliveryMetaKey) {
		t.Fatalf("later prompt detached or leaked private metadata: %s %v", frame, err)
	}
}

func TestDeliveryJournalReclaimsOnlyAcknowledgedSettledEvidence(t *testing.T) {
	j, err := newDeliveryJournal(filepath.Join(t.TempDir(), "journal"), "epoch")
	if err != nil {
		t.Fatal(err)
	}
	defer j.file.Close()
	for i := range maxDeliveryRecords + 1 {
		attempt := fmt.Sprintf("attempt-%d", i)
		if _, err := j.prepare([]byte(fmt.Sprintf(`{"id":%d,"method":"session/prompt","params":{"sessionId":"session","_meta":{"aoDeliveryAttempt":%q}}}`, i, attempt))); err != nil {
			t.Fatal(err)
		}
		if err := j.acknowledge([]ports.ChatDeliveryLookup{{AttemptID: attempt, ProviderEpoch: "epoch", ProviderRequestID: fmt.Sprint(i)}}); !errors.Is(err, ports.ErrChatDeliveryUnverified) {
			t.Fatal("in-flight evidence was evicted")
		}
		if err := j.observe([]byte(fmt.Sprintf(`{"id":%d,"result":{"stopReason":"end_turn"}}`, i))); err != nil {
			t.Fatal(err)
		}
		key := []ports.ChatDeliveryLookup{{AttemptID: attempt, ProviderEpoch: "epoch", ProviderRequestID: fmt.Sprint(i)}}
		if err := j.acknowledge(key); err != nil {
			t.Fatal(err)
		}
	}
	if len(j.records) != 0 || len(j.requests) != 0 || len(j.waiting) != 0 {
		t.Fatalf("settled host state grew: records=%d requests=%d", len(j.records), len(j.requests))
	}
	j.size = maxDeliveryJournalBytes
	if _, err := j.prepare([]byte(`{"id":99999,"method":"turn/start","params":{"_meta":{"aoDeliveryAttempt":"after-compaction"}}}`)); err != nil {
		t.Fatal(err)
	}
	if j.size > 4096 {
		t.Fatalf("journal did not compact acknowledged history: %d", j.size)
	}
	if _, err := j.query([]ports.ChatDeliveryLookup{{AttemptID: "after-compaction"}}, true); err != nil {
		t.Fatal(err)
	}
}

func TestDeliveryPrepareFailureStillStripsPrivateMetadata(t *testing.T) {
	j, err := newDeliveryJournal(filepath.Join(t.TempDir(), "journal"), "epoch")
	if err != nil {
		t.Fatal(err)
	}
	_ = j.file.Close()
	frame, err := j.prepare([]byte(`{"id":1,"method":"turn/start","params":{"_meta":{"aoDeliveryAttempt":"failed-attempt"},"input":[]}}`))
	if err == nil || len(frame) == 0 || strings.Contains(string(frame), DeliveryMetaKey) {
		t.Fatalf("failed evidence layer changed native request: %s %v", frame, err)
	}
	if _, err := j.query(nil, true); !errors.Is(err, ports.ErrChatDeliveryUnverified) {
		t.Fatalf("failed journal claimed settled: %v", err)
	}
}

func TestDeliveryCheckpointFailureRecoversWithoutLosingNativeReceipt(t *testing.T) {
	j, err := newDeliveryJournal(filepath.Join(t.TempDir(), "journal"), "epoch")
	if err != nil {
		t.Fatal(err)
	}
	writable := j.file
	defer writable.Close()
	readonly, err := os.Open(writable.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer readonly.Close()
	j.file = readonly
	_, err = j.prepare([]byte(`{"id":1,"method":"session/prompt","params":{"sessionId":"session","_meta":{"aoDeliveryAttempt":"attempt"}}}`))
	if err == nil {
		t.Fatal("fault did not reach checkpoint write")
	}
	if err := j.observe([]byte(`{"id":1,"result":{"stopReason":"end_turn"}}`)); err == nil {
		t.Fatal("checkpoint unexpectedly writable")
	}
	key := []ports.ChatDeliveryLookup{{AttemptID: "attempt", ProviderEpoch: "epoch", ProviderRequestID: "1"}}
	receipt, err := j.query(key, false)
	if err != nil || len(receipt) != 1 || !receipt[0].ExecutionSettled {
		t.Fatalf("checkpoint failure discarded native evidence: %+v %v", receipt, err)
	}
	if _, err := j.query(key, true); err == nil {
		t.Fatal("unwritable checkpoint reported flushed")
	}
	if err := j.acknowledge(key); err != nil {
		t.Fatalf("checkpoint failure blocked durable consumer ACK: %v", err)
	}
	j.file = writable
	if _, err := j.query(nil, true); err != nil {
		t.Fatalf("idle host did not recover after writable checkpoint: %v", err)
	}
	if _, err := j.prepare([]byte(`{"id":2,"method":"session/prompt","params":{"sessionId":"session","_meta":{"aoDeliveryAttempt":"later"}}}`)); err != nil {
		t.Fatal(err)
	}
	if err := j.observe([]byte(`{"id":2,"result":{"stopReason":"end_turn"}}`)); err != nil {
		t.Fatal(err)
	}
	got, err := j.query([]ports.ChatDeliveryLookup{{AttemptID: "later"}}, true)
	if err != nil || !got[0].ExecutionSettled {
		t.Fatalf("later turn remained poisoned: %+v %v", got, err)
	}
}

func TestDeliveryCapacityReclaimsSettledWithoutPoisoningHost(t *testing.T) {
	j, err := newDeliveryJournal(filepath.Join(t.TempDir(), "journal"), "epoch")
	if err != nil {
		t.Fatal(err)
	}
	defer j.file.Close()
	for i := range maxDeliveryRecords {
		key := fmt.Sprintf("settled-%d", i)
		j.records[key] = deliveryRecord{Receipt: domain.ChatDeliveryReceipt{AttemptID: key, ExecutionSettled: true, ObservedAt: time.Now()}}
	}
	if _, err := j.prepare([]byte(`{"id":1,"method":"session/prompt","params":{"sessionId":"session","_meta":{"aoDeliveryAttempt":"new"}}}`)); err != nil {
		t.Fatalf("settled cache poisoned new turn: %v", err)
	}
	if err := j.observe([]byte(`{"id":1,"result":{"stopReason":"end_turn"}}`)); err != nil {
		t.Fatal(err)
	}
	receipt, err := j.query([]ports.ChatDeliveryLookup{{AttemptID: "new"}}, true)
	if err != nil || !receipt[0].ExecutionSettled {
		t.Fatalf("capacity poison persisted: %+v %v", receipt, err)
	}
}
