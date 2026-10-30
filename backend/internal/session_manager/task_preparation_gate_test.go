package sessionmanager

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type preparationRestoreRuntime struct {
	*fakeRuntime
	created chan struct{}
	release chan struct{}
}

func (r *preparationRestoreRuntime) Create(ctx context.Context, cfg ports.RuntimeConfig) (ports.RuntimeHandle, error) {
	handle, err := r.fakeRuntime.Create(ctx, cfg)
	close(r.created)
	<-r.release
	return handle, err
}

type preparationGateWorkspace struct {
	*fakeWorkspace
	marker      string
	prepDone    <-chan struct{}
	destroyed   atomic.Int64
	destroyDone chan struct{}
	once        sync.Once
}

func (w *preparationGateWorkspace) Create(ctx context.Context, cfg ports.WorkspaceConfig) (ports.WorkspaceInfo, error) {
	w.prepDone = ctx.Done()
	return w.fakeWorkspace.Create(ctx, cfg)
}

func (w *preparationGateWorkspace) Destroy(context.Context, ports.WorkspaceInfo) error {
	w.destroyed.Add(1)
	err := os.Remove(w.marker)
	w.once.Do(func() { close(w.destroyDone) })
	return err
}

func TestTaskPreparationCancelAndExpiryWaitForPublicRestore(t *testing.T) {
	for _, action := range []string{"cancel", "expiry", "startup", "reconcile"} {
		t.Run(action, func(t *testing.T) {
			f := newTUISettingsFixture(t, domain.HarnessCodex)
			m := f.m
			m.runBackground = func(work func()) { work() }
			m.taskPreparationTTL = time.Hour
			rec := f.rec
			rec.Metadata.Branch = "ao/source"
			rec.Metadata.Prompt = "resume task"
			if err := f.store.UpdateSession(t.Context(), rec); err != nil {
				t.Fatal(err)
			}
			if err := f.lifecycle.MarkTerminated(t.Context(), rec.ID); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(rec.Metadata.WorkspacePath, "retained-work.txt")
			ws := &preparationGateWorkspace{fakeWorkspace: &fakeWorkspace{path: rec.Metadata.WorkspacePath}, marker: marker, destroyDone: make(chan struct{})}
			m.workspace = ws
			project, _, err := f.store.GetProject(t.Context(), string(rec.ProjectID))
			if err != nil {
				t.Fatal(err)
			}
			token, err := m.PrepareTaskWorkspace(t.Context(), project)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				m.taskPreparationsMu.Lock()
				defer m.taskPreparationsMu.Unlock()
				for _, prep := range m.taskPreparations {
					prep.timer.Stop()
					prep.cancel()
				}
			})
			if err := os.WriteFile(marker, []byte("restore owns this workspace"), 0o600); err != nil {
				t.Fatal(err)
			}
			rt := &preparationRestoreRuntime{fakeRuntime: f.runtime, created: make(chan struct{}), release: make(chan struct{})}
			m.runtime = rt
			restoreDone := make(chan error, 1)
			go func() {
				_, err := m.RestoreWithMode(t.Context(), rec.ID)
				restoreDone <- err
			}()
			releaseRestore := sync.OnceFunc(func() { close(rt.release) })
			defer releaseRestore()
			select {
			case <-rt.created:
			case err := <-restoreDone:
				t.Fatalf("Restore must reach Runtime.Create: %v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("Restore did not reach Runtime.Create")
			}
			cancelDone := make(chan error, 1)
			started := ws.prepDone
			switch action {
			case "cancel":
				go func() { cancelDone <- m.CancelTaskPreparation(t.Context(), token) }()
			case "expiry":
				m.taskPreparationsMu.Lock()
				m.taskPreparations[token].timer.Reset(time.Millisecond)
				m.taskPreparationsMu.Unlock()
			default:
				entered := make(chan struct{})
				started = entered
				if action == "reconcile" {
					prep, _, err := f.store.GetSession(t.Context(), domain.SessionID(token))
					if err != nil {
						t.Fatal(err)
					}
					m.startupTaskPreparations = []domain.SessionRecord{prep}
				}
				go func() {
					close(entered)
					if action == "startup" {
						cancelDone <- m.CleanupInterruptedTaskPreparations(t.Context())
					} else {
						cancelDone <- m.ReconcileBackground(t.Context())
					}
				}()
			}
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("cleanup did not start")
			}
			removedWhileRestoring := false
			select {
			case <-ws.destroyDone:
				removedWhileRestoring = true
			case <-time.After(200 * time.Millisecond):
			}
			releaseRestore()
			if err := <-restoreDone; err != nil {
				t.Fatalf("Restore failed: %v", err)
			}
			if action != "expiry" {
				err := <-cancelDone
				if action == "cancel" && err == nil {
					t.Error("Cancel must retain the newly-live session's workspace")
				} else if action != "cancel" && err != nil {
					t.Fatalf("startup cleanup failed: %v", err)
				}
			} else {
				deadline := time.Now().Add(5 * time.Second)
				for {
					m.taskPreparationsMu.Lock()
					prep := m.taskPreparations[token]
					finished := prep == nil || !prep.cleaning
					m.taskPreparationsMu.Unlock()
					if finished {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("expiry did not finish after Restore released its gate")
					}
					time.Sleep(time.Millisecond)
				}
			}
			data, readErr := os.ReadFile(marker)
			live, _, err := f.store.GetSession(t.Context(), rec.ID)
			_, prepExists, prepErr := f.store.GetSession(t.Context(), domain.SessionID(token))
			if removedWhileRestoring || ws.destroyed.Load() != 0 || readErr != nil || string(data) != "restore owns this workspace" || !prepExists || prepErr != nil || err != nil || live.IsTerminated || f.runtime.created != 1 {
				t.Fatalf("Restore/Cancel gate bypass: removedWhileRestore=%v Destroy=%d marker=%q readErr=%v prepExists=%v prepErr=%v terminated=%v restoreErr=%v runtimeCreate=%d", removedWhileRestoring, ws.destroyed.Load(), data, readErr, prepExists, prepErr, live.IsTerminated, err, f.runtime.created)
			}
		})
	}
}

func TestDiscardTaskPreparationTransfersHeldSpawnWorkspaceGate(t *testing.T) {
	m, st, _, ws := newManager()
	m.dataDir = t.TempDir()
	m.runBackground = func(work func()) { work() }
	token, err := m.PrepareTaskWorkspace(t.Context(), st.projects["mer"])
	if err != nil {
		t.Fatal(err)
	}
	prep := m.claimTaskPreparation(token, "mer")
	if prep == nil {
		t.Fatal("preparation was not claimable")
	}
	release := sync.OnceFunc(m.acquireWorkspaceGate("mer"))
	defer release()
	ctx := context.WithValue(t.Context(), spawnWorkspaceGateKey{}, release)
	done := make(chan struct{})
	go func() {
		m.discardClaimedTaskPreparation(ctx, prep)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("discard tried to reacquire its own non-reentrant workspace gate")
	}
	if ws.destroyed != 1 {
		t.Fatalf("discard did not reclaim its own unowned preparation: Destroy=%d", ws.destroyed)
	}
	if _, exists := st.sessions[domain.SessionID(token)]; exists {
		t.Fatal("discard retained the unused preparation row")
	}
}
