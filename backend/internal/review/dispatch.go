package review

import (
	"context"
	"sync"
)

type reviewWorkerState struct {
	operation sync.Mutex

	dispatchMu  sync.Mutex
	cancelEpoch uint64
	cancellers  int
	cancelDone  chan struct{}
}

// beginCancellation invalidates outstanding dispatch reads before Cancel
// inspects the active passes. Only the admission check shares this mutex.
func (w *reviewWorkerState) beginCancellation() func() {
	w.dispatchMu.Lock()
	w.cancelEpoch++
	if w.cancellers == 0 {
		w.cancelDone = make(chan struct{})
	}
	w.cancellers++
	w.dispatchMu.Unlock()
	return func() {
		w.dispatchMu.Lock()
		w.cancellers--
		if w.cancellers == 0 {
			close(w.cancelDone)
			w.cancelDone = nil
		}
		w.dispatchMu.Unlock()
	}
}

func (w *reviewWorkerState) admitDispatch(ctx context.Context, read func() error) error {
	for {
		w.dispatchMu.Lock()
		epoch, done := w.cancelEpoch, w.cancelDone
		w.dispatchMu.Unlock()
		if done != nil {
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := read(); err != nil {
			return err
		}
		w.dispatchMu.Lock()
		// Linearize delivery before the launcher call. A later Cancel can
		// respond while I/O is in flight and settle the returned handle.
		admitted := epoch == w.cancelEpoch
		w.dispatchMu.Unlock()
		if admitted {
			return nil
		}
	}
}
