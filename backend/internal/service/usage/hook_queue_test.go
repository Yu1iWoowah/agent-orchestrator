package usage

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

type blockingHookRecorder struct {
	release chan struct{}
	entered chan struct{}
	mu      sync.Mutex
	events  []string
}

func (r *blockingHookRecorder) RecordHook(ctx context.Context, _ domain.SessionID, signal HookSignal) error {
	if r.entered != nil {
		select {
		case r.entered <- struct{}{}:
		default:
		}
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-r.release:
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	r.events = append(r.events, signal.Event)
	r.mu.Unlock()
	return nil
}

func (r *blockingHookRecorder) FinalizeSession(context.Context, domain.SessionID, string, int64) error {
	r.mu.Lock()
	r.events = append(r.events, "finalize")
	r.mu.Unlock()
	return nil
}

func (r *blockingHookRecorder) ReactivateSession(context.Context, domain.SessionID, string) error {
	return nil
}

func (r *blockingHookRecorder) recorded() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

func TestHookQueueReturnsWhileRecorderIsBusyAndKeepsOrder(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	recorder := &blockingHookRecorder{release: make(chan struct{})}
	queue := NewHookQueue(recorder, nil)
	go queue.Run(ctx)

	want := []string{"session-start", "user-prompt-submit", "pre-tool-use", "post-tool-use", "stop"}
	start := time.Now()
	for _, event := range want {
		if err := queue.RecordHook(ctx, "s-1", HookSignal{Event: event}); err != nil {
			t.Fatalf("RecordHook(%s): %v", event, err)
		}
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("RecordHook waited %s for a busy recorder", elapsed)
	}

	close(recorder.release)
	deadline := time.Now().Add(5 * time.Second)
	for {
		recorder.mu.Lock()
		got := append([]string(nil), recorder.events...)
		recorder.mu.Unlock()
		if len(got) == len(want) {
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("recorded order %v, want %v", got, want)
				}
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("recorded %v, want %v", got, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestHookQueueFinalizeRecordsQueuedSignalsFirst(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	recorder := &blockingHookRecorder{release: make(chan struct{})}
	queue := NewHookQueue(recorder, nil)
	go queue.Run(ctx)

	for _, event := range []string{"session-start", "user-prompt-submit"} {
		if err := queue.RecordHook(ctx, "s-1", HookSignal{Event: event}); err != nil {
			t.Fatalf("RecordHook(%s): %v", event, err)
		}
	}
	finalized := make(chan error, 1)
	go func() { finalized <- queue.FinalizeSession(ctx, "s-1", "launch-1", 7) }()
	select {
	case err := <-finalized:
		t.Fatalf("FinalizeSession returned before the queued signals were recorded: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(recorder.release)
	select {
	case err := <-finalized:
		if err != nil {
			t.Fatalf("FinalizeSession: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("FinalizeSession did not return")
	}
	if got, want := recorder.recorded(), []string{"session-start", "user-prompt-submit", "finalize"}; !slices.Equal(got, want) {
		t.Fatalf("recorded %v, want %v", got, want)
	}
}

func TestHookQueueFinalizeDoesNotWaitAfterRunStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	recorder := &blockingHookRecorder{release: make(chan struct{})}
	queue := NewHookQueue(recorder, nil)
	stopped := make(chan struct{})
	go func() {
		queue.Run(ctx)
		close(stopped)
	}()
	cancel()
	<-stopped

	finalized := make(chan error, 1)
	go func() { finalized <- queue.FinalizeSession(context.Background(), "s-1", "launch-1", 7) }()
	select {
	case err := <-finalized:
		if err != nil {
			t.Fatalf("FinalizeSession: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("FinalizeSession waited for a stopped queue")
	}
	if got, want := recorder.recorded(), []string{"finalize"}; !slices.Equal(got, want) {
		t.Fatalf("recorded %v, want %v", got, want)
	}
}

func TestHookQueueDrainsAcceptedSignalsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	recorder := &blockingHookRecorder{release: make(chan struct{}), entered: make(chan struct{}, 1)}
	release := sync.OnceFunc(func() { close(recorder.release) })
	defer release()
	queue := NewHookQueue(recorder, nil)
	go queue.Run(ctx)

	testCtx, endTest := context.WithTimeout(t.Context(), 5*time.Second)
	defer endTest()
	if err := queue.RecordHook(testCtx, "s-1", HookSignal{Event: "session-start"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-recorder.entered:
	case <-testCtx.Done():
		t.Fatal("recorder did not start")
	}
	if err := queue.RecordHook(testCtx, "s-1", HookSignal{Event: "subagent-stop", ProviderHint: "anthropic", SubagentTranscriptPath: "subagent.jsonl"}); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-queue.closing:
	case <-testCtx.Done():
		t.Fatal("cancellation did not close admission while the recorder was blocked")
	}
	if err := queue.RecordHook(testCtx, "s-1", HookSignal{Event: "late"}); !errors.Is(err, ErrHookQueueClosing) {
		t.Fatalf("late RecordHook error = %v, want %v", err, ErrHookQueueClosing)
	}

	joined := make(chan error, 1)
	go func() { joined <- queue.CloseAndWait() }()
	finalizeCtx, cancelFinalize := context.WithTimeout(testCtx, 50*time.Millisecond)
	defer cancelFinalize()
	if err := queue.FinalizeSession(finalizeCtx, "s-1", "launch-1", 7); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("FinalizeSession before drain = %v, want deadline exceeded", err)
	}
	select {
	case err := <-joined:
		t.Fatalf("worker joined before drain: %v", err)
	default:
	}

	release()
	select {
	case err := <-joined:
		if err != nil {
			t.Fatalf("CloseAndWait: %v", err)
		}
	case <-testCtx.Done():
		t.Fatal("worker did not join after the recorder was released")
	}
	if err := queue.FinalizeSession(testCtx, "s-1", "launch-1", 7); err != nil {
		t.Fatal(err)
	}
	if got, want := recorder.recorded(), []string{"session-start", "subagent-stop", "finalize"}; !slices.Equal(got, want) {
		t.Fatalf("recorded %v, want %v", got, want)
	}
	if err := queue.RecordHook(testCtx, "s-1", HookSignal{Event: "late"}); !errors.Is(err, ErrHookQueueClosing) {
		t.Fatalf("RecordHook after drain error = %v, want %v", err, ErrHookQueueClosing)
	}
}
