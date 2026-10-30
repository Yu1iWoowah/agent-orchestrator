package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestReplacementReconcileWaitsForStartupAndCancels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	runReplacementReconciliation(ctx, make(chan struct{}), make(chan time.Time), func(context.Context) error { calls++; return nil }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if calls != 0 {
		t.Fatal("replacement ran before startup or after cancellation", calls)
	}
}
func TestReplacementReconcileRunsAfterStartupAndSurvivesStepFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startup := make(chan struct{})
	ticks := make(chan time.Time)
	calls := make(chan struct{}, 2)
	done := make(chan struct{})
	var logs strings.Builder
	go func() {
		defer close(done)
		runReplacementReconciliation(ctx, startup, ticks, func(context.Context) error {
			calls <- struct{}{}
			return errors.New("transient replacement read failure")
		}, slog.New(slog.NewTextHandler(&logs, nil)))
	}()
	select {
	case <-calls:
		t.Fatal("replacement ran before startup")
	default:
	}
	close(startup)
	await := func(ch <-chan struct{}) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatal("replacement loop did not make progress")
		}
	}
	await(calls)
	select {
	case ticks <- time.Now():
	case <-time.After(5 * time.Second):
		t.Fatal("replacement loop stopped after error")
	}
	await(calls)
	cancel()
	await(done)
	if !strings.Contains(logs.String(), "transient replacement read failure") {
		t.Fatal("reconciliation failure not observable")
	}
}
