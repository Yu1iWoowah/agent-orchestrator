package usage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestPipelineRetriesWatcherCreation(t *testing.T) {
	pipeline := NewPipeline(
		&coordinatorTestStore{},
		coordinatorTestIngestor(func(context.Context, int64) (IngestResult, error) {
			return IngestResult{}, nil
		}),
		[]string{t.TempDir()},
		CoordinatorConfig{Workers: 1},
		TranscriptWatcherConfig{},
	)
	pipeline.restartWait = time.Millisecond
	created := make(chan struct{})
	var calls atomic.Int64
	pipeline.newWatcher = func(context.Context, []string) (transcriptWatcher, error) {
		if calls.Add(1) == 1 {
			return nil, errors.New("temporary watcher failure")
		}
		select {
		case <-created:
		default:
			close(created)
		}
		return newCoordinatorTestWatcher(), nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := pipeline.Start(ctx)
	waitForCoordinatorSignal(t, created, "pipeline did not retry watcher creation")
	cancel()
	waitForCoordinatorSignal(t, done, "pipeline did not stop")
	if got := calls.Load(); got < 2 {
		t.Fatalf("watcher creation calls = %d, want at least 2", got)
	}
}

func TestPipelineAppliesWatcherRootBoundary(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	path := filepath.Join(root, "session.jsonl")
	mustNoError(t, os.WriteFile(path, []byte("{}\n"), 0o600))
	pipeline := NewPipeline(nil, nil, []string{root}, CoordinatorConfig{}, TranscriptWatcherConfig{
		ValidateRoot: func(context.Context, string) error { return errors.New("rejected history root") },
	})
	watcher, err := pipeline.newWatcher(ctx, pipeline.roots)
	mustNoError(t, err)
	actual := watcher.(*TranscriptWatcher)
	t.Cleanup(actual.close)
	mustNoError(t, actual.Rebuild(ctx, []string{path}))
	if len(actual.watched) != 0 {
		t.Fatal("pipeline watched a source rejected by its root boundary")
	}
}
