package usage

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"sync"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// HookQueue records hook usage signals in arrival order without holding the
// hook's HTTP request. A usage scan can hold the collector for seconds, and the
// agent waits for each hook before its next prompt or tool call.
type HookQueue struct {
	next    hookRecorder
	signals chan queuedHookSignal
	closing chan struct{}
	stopped chan struct{}
	log     *slog.Logger

	closeOnce sync.Once
	senders   sync.RWMutex
	drained   bool // Published by closing stopped.

	abortCtx  context.Context
	abort     context.CancelFunc
	abortOnce sync.Once
	pendingMu sync.Mutex
	pending   map[domain.SessionID]int
}

var (
	// ErrHookQueueClosing means the signal was not accepted.
	ErrHookQueueClosing   = errors.New("usage hook queue is closing")
	errHookQueueUndrained = errors.New("usage hook worker stopped before draining")
)

type hookRecorder interface {
	RecordHook(ctx context.Context, id domain.SessionID, signal HookSignal) error
	FinalizeSession(ctx context.Context, id domain.SessionID, expectedRuntimeLaunchID string, expectedSessionRevision int64) error
	ReactivateSession(ctx context.Context, id domain.SessionID, expectedRuntimeLaunchID string) error
}

type queuedHookSignal struct {
	id     domain.SessionID
	signal HookSignal
	// flushed marks a flush point: Run closes it instead of recording.
	flushed chan struct{}
}

const hookQueueSize = 1024

// NewHookQueue returns a queue in front of next. Run must be started for
// queued signals to be recorded.
func NewHookQueue(next hookRecorder, log *slog.Logger) *HookQueue {
	if log == nil {
		log = slog.Default()
	}
	abortCtx, abort := context.WithCancel(context.Background())
	return &HookQueue{
		next:     next,
		signals:  make(chan queuedHookSignal, hookQueueSize),
		closing:  make(chan struct{}),
		stopped:  make(chan struct{}),
		log:      log,
		abortCtx: abortCtx,
		abort:    abort,
		pending:  make(map[domain.SessionID]int),
	}
}

// RecordHook queues the signal. A full queue makes the caller wait for space
// rather than record out of order.
func (q *HookQueue) RecordHook(ctx context.Context, id domain.SessionID, signal HookSignal) error {
	return q.enqueue(ctx, queuedHookSignal{id: id, signal: signal})
}

func (q *HookQueue) enqueue(ctx context.Context, item queuedHookSignal) error {
	q.senders.RLock()
	defer q.senders.RUnlock()
	select {
	case <-q.closing:
		return ErrHookQueueClosing
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if item.flushed == nil {
		q.pendingMu.Lock()
		q.pending[item.id]++
		q.pendingMu.Unlock()
	}
	var err error
	select {
	case q.signals <- item:
		return nil
	case <-q.closing:
		err = ErrHookQueueClosing
	case <-ctx.Done():
		err = ctx.Err()
	}
	if item.flushed == nil {
		q.complete(item.id)
	}
	return err
}

func (q *HookQueue) complete(id domain.SessionID) {
	q.pendingMu.Lock()
	defer q.pendingMu.Unlock()
	q.pending[id]--
	if q.pending[id] == 0 {
		delete(q.pending, id)
	}
}
func (q *HookQueue) closeAdmission() {
	q.closeOnce.Do(func() {
		// Wake callers waiting for space before taking the sender lock.
		close(q.closing)
		q.senders.Lock()
		close(q.signals)
		q.senders.Unlock()
	})
}

// Done closes when Run exits and no longer uses the recorder.
func (q *HookQueue) Done() <-chan struct{} { return q.stopped }

// CloseAndWait stops admission and waits for the drain within ctx's budget.
// Cancellation stops the recorder and reports any events still outstanding.
func (q *HookQueue) CloseAndWait(ctx context.Context) error {
	q.closeAdmission()
	select {
	case <-q.stopped:
	case <-ctx.Done():
		select {
		case <-q.stopped:
		default:
			q.abortDrain(ctx.Err())
			return ctx.Err()
		}
	}
	if !q.drained {
		q.abortDrain(errHookQueueUndrained)
		return errHookQueueUndrained
	}
	return nil
}

func (q *HookQueue) abortDrain(err error) {
	q.abortOnce.Do(func() {
		q.pendingMu.Lock()
		pending := maps.Clone(q.pending)
		q.abort()
		q.pendingMu.Unlock()
		total := 0
		for _, count := range pending {
			total += count
		}
		q.log.Warn("usage hook drain incomplete", "err", err, "pending_events", total, "pending_by_session", pending)
	})
}

// FinalizeSession records every signal queued before it, then finalizes. The
// collector drops signals for a terminated session, so a session terminated
// while its signals wait behind a usage scan would otherwise lose its usage.
func (q *HookQueue) FinalizeSession(
	ctx context.Context,
	id domain.SessionID,
	expectedRuntimeLaunchID string,
	expectedSessionRevision int64,
) error {
	flushed := make(chan struct{})
	if err := q.enqueue(ctx, queuedHookSignal{flushed: flushed}); err != nil {
		if !errors.Is(err, ErrHookQueueClosing) {
			return err
		}
		// The worker must drain everything it accepted before finalization.
		flushed = nil
	}
	select {
	case <-flushed:
	case <-q.stopped:
		if !q.drained {
			return errHookQueueUndrained
		}
	case <-ctx.Done():
		return ctx.Err()
	}
	return q.next.FinalizeSession(ctx, id, expectedRuntimeLaunchID, expectedSessionRevision)
}

// ReactivateSession passes through. Lifecycle takes its reactivator from the
// finalizer, so the queue must provide both.
func (q *HookQueue) ReactivateSession(ctx context.Context, id domain.SessionID, expectedRuntimeLaunchID string) error {
	return q.next.ReactivateSession(ctx, id, expectedRuntimeLaunchID)
}

// Run records queued signals in order. Cancellation closes admission even
// while a recording is in flight, and all accepted signals are drained.
func (q *HookQueue) Run(ctx context.Context) {
	defer close(q.stopped)
	defer q.closeAdmission()
	stopWatching := context.AfterFunc(ctx, q.closeAdmission)
	defer stopWatching()

	// A hook has already been acknowledged when it reaches the recorder.
	// Shutdown must also let the recording in progress finish.
	recordCtx, cancelRecord := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelRecord()
	stopAbort := context.AfterFunc(q.abortCtx, cancelRecord)
	defer stopAbort()
	for item := range q.signals {
		if q.abortCtx.Err() != nil {
			return
		}
		if item.flushed != nil {
			close(item.flushed)
			continue
		}
		err := q.next.RecordHook(recordCtx, item.id, item.signal)
		q.complete(item.id)
		if err != nil && !errors.Is(err, ErrUsageSessionNotFound) && q.abortCtx.Err() == nil {
			q.log.Warn("usage hook processing failed", "session", item.id, "event", item.signal.Event, "err", err)
		}
	}
	q.drained = q.abortCtx.Err() == nil
}
