package persistenthost

import (
	"bufio"
	"context"
	"errors"
	"os"
	"path/filepath"
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
	if err = j.observe([]byte(`{"id":12,"result":{}}`)); err != nil {
		t.Fatal(err)
	}
	unmatched, _ := j.query(lookup, false)
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
