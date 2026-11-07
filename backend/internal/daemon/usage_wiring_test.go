package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	usagepipeline "github.com/aoagents/agent-orchestrator/backend/internal/observe/usage"
	usagesvc "github.com/aoagents/agent-orchestrator/backend/internal/service/usage"
)

func TestUsagePipelineWatchRootsIncludesManagedWrites(t *testing.T) {
	stateDir := t.TempDir()
	history := filepath.Join(stateDir, "agent-accounts", "history")
	roots := usagesvc.SourceRoots{
		ClaudeProjects:        filepath.Join(stateDir, "device", "claude", "projects"),
		CodexSessions:         filepath.Join(stateDir, "device", "codex", "sessions"),
		CodexArchived:         filepath.Join(stateDir, "device", "codex", "archived_sessions"),
		KimiHome:              filepath.Join(stateDir, "kimi"),
		AccountClaudeProjects: filepath.Join(history, "claude", "projects"),
		AccountCodexSessions:  filepath.Join(history, "codex", "sessions"),
		AccountCodexArchived:  filepath.Join(history, "codex", "archived_sessions"),
	}
	for _, test := range []struct {
		name string
		root string
		path string
	}{
		{"kimi", roots.KimiHome, filepath.Join("sessions", "native-1", "agents", "main", "wire.jsonl")},
		{"account claude", roots.AccountClaudeProjects, filepath.Join("project", "native-1.jsonl")},
		{"account codex sessions", roots.AccountCodexSessions, filepath.Join("2026", "11", "08", "rollout-native-1.jsonl")},
		{"account codex archived", roots.AccountCodexArchived, "rollout-native-1.jsonl"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(test.root, test.path)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			watcher, err := usagepipeline.NewTranscriptWatcher(
				context.Background(), usagePipelineWatchRoots(roots),
			)
			if err != nil {
				t.Fatalf("create usage watcher: %v", err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := watcher.Start(ctx)
			t.Cleanup(func() {
				cancel()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Error("usage watcher did not stop")
				}
			})
			if err := watcher.Rebuild(ctx, []string{path}); err != nil {
				t.Fatalf("rebuild usage watcher: %v", err)
			}
			file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				t.Fatal(err)
			}
			_, writeErr := file.WriteString("{\"updated\":true}\n")
			closeErr := file.Close()
			if writeErr != nil || closeErr != nil {
				t.Fatalf("append transcript: write=%v close=%v", writeErr, closeErr)
			}
			wantPath, err := filepath.EvalSymlinks(path)
			if err != nil {
				t.Fatal(err)
			}
			select {
			case event := <-watcher.Events():
				if event.Path != wantPath || event.Discovery {
					t.Fatalf("usage event = %+v, want append event for %q", event, wantPath)
				}
			case err := <-watcher.Errors():
				t.Fatalf("usage watcher error: %v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("transcript append did not reach the usage watcher")
			}
		})
	}
}
