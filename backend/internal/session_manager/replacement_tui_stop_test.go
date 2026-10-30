package sessionmanager

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type replacementStopRuntime struct {
	*replacementTUIRuntime
	keepAlive bool
	afterStop func()
}

func (s *replacementTUIStore) SetReplacementTUIRuntimeReleased(ctx context.Context, operation string, revision int64, id domain.SessionID, sessionRevision int64) (bool, error) {
	rec, found, err := s.GetSession(ctx, id)
	if err != nil || !found || s.operation.ID != operation || s.operation.Revision != revision || rec.Revision != sessionRevision || !s.operation.AllowsTUIQuiescence(rec) {
		return false, err
	}
	return s.SetSessionRuntimeReleased(ctx, id, rec.Metadata.RuntimeLaunchID)
}

func (r *replacementStopRuntime) Destroy(ctx context.Context, handle ports.RuntimeHandle) error {
	err := r.fakeRuntime.Destroy(ctx, handle)
	if r.keepAlive {
		r.aliveByHandle[handle.ID] = true
	}
	if r.afterStop != nil {
		r.afterStop()
	}
	return err
}

func TestReplacementTUIStopRequiresPositiveOwnedDeath(t *testing.T) {
	for _, which := range []string{"stopped", "destroy nil but alive", "unknown after destroy", "rotated after destroy", "release then crash", "unreleased dead", "released alive", "busy", "workload", "missing intent"} {
		t.Run(which, func(t *testing.T) {
			f := newReplacementTUIFixture(t)
			f.acceptControl()
			f.writeNative(t, true, false, "")
			now := time.Now().UTC()
			f.op.Phase = domain.ReplacementExportingTranscript
			f.op.FrozenAt, f.op.SourceStopIntentAt, f.op.Preparation.VerifiedAt = &now, &now, &now
			f.op.Transcript = &domain.ReplacementTranscript{AttemptID: "evidence-attempt"}
			runtime := &replacementStopRuntime{replacementTUIRuntime: f.runtime}
			f.manager.runtime = runtime
			rec := f.store.sessions[f.op.SourceSessionID]
			switch which {
			case "destroy nil but alive":
				runtime.keepAlive = true
			case "unknown after destroy":
				runtime.afterStop = func() { runtime.aliveErr = errors.New("unknown liveness") }
			case "rotated after destroy":
				runtime.afterStop = func() {
					changed := f.store.sessions[rec.ID]
					changed.Metadata.BrowserCapabilityVerifier = "rotated"
					f.store.sessions[rec.ID] = changed
				}
			case "release then crash":
				rec.Metadata.RuntimeReleasedLaunchID = rec.Metadata.RuntimeLaunchID
				runtime.aliveByHandle["h1"] = false
			case "unreleased dead":
				runtime.aliveByHandle["h1"] = false
			case "released alive":
				rec.Metadata.RuntimeReleasedLaunchID = rec.Metadata.RuntimeLaunchID
			case "busy":
				rec.Activity.State = domain.ActivityActive
			case "workload":
				runtime.commands = 1
			case "missing intent":
				f.op.SourceStopIntentAt = nil
			}
			f.store.sessions[rec.ID], f.store.operation = rec, f.op
			stop, err := f.manager.QuiesceReplacementSource(context.Background(), f.op)
			current := f.store.sessions[rec.ID]
			if which == "stopped" || which == "release then crash" {
				if err != nil || !stop.MatchesSource(current) || current.IsTerminated || current.Metadata.WorkspacePath != rec.Metadata.WorkspacePath {
					t.Fatalf("owned source shutdown not confirmed/preserved: %+v %+v %v", stop, current, err)
				}
				if _, err := f.manager.QuiesceReplacementSource(context.Background(), f.op); err != nil {
					t.Fatalf("confirmed dead launch cannot reconcile: %v", err)
				}
			} else if err == nil || !stop.ConfirmedAt.IsZero() {
				t.Fatalf("%s fabricated stop receipt: %+v %v", which, stop, err)
			}
			wantStops := 0
			if which == "stopped" || which == "destroy nil but alive" || which == "unknown after destroy" || which == "rotated after destroy" {
				wantStops = 1
			}
			if runtime.destroyed != wantStops {
				t.Fatalf("%s stop calls=%d want=%d", which, runtime.destroyed, wantStops)
			}
			if (which == "busy" || which == "workload") && !errors.Is(err, domain.ErrReplacementSourceBusy) {
				t.Fatalf("busy result cannot be safely retried: %v", err)
			}
		})
	}
}

func TestReplacementTUIStopDrainsOnlySourceInputAndPreservesWorkersAndWorkspace(t *testing.T) {
	f := newReplacementTUIFixture(t)
	f.acceptControl()
	f.writeNative(t, true, false, "")
	now := time.Now().UTC()
	f.op.Phase, f.op.FrozenAt, f.op.SourceStopIntentAt = domain.ReplacementExportingTranscript, &now, &now
	f.op.Preparation.VerifiedAt = &now
	f.op.Transcript = &domain.ReplacementTranscript{AttemptID: "evidence-attempt"}
	f.store.operation = f.op
	source := f.store.sessions[f.op.SourceSessionID]
	source.Metadata.WorkspacePath = t.TempDir()
	f.store.sessions[source.ID] = source
	file := filepath.Join(source.Metadata.WorkspacePath, "untracked.txt")
	if err := os.WriteFile(file, []byte("preserve these bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	worker := mkLive("mer-worker")
	worker.Kind, worker.Harness = domain.KindWorker, domain.HarnessCodex
	worker.Metadata.RuntimeHandleID, worker.Metadata.AgentSessionID = "worker-runtime", "worker-native"
	f.store.sessions[worker.ID] = worker
	f.runtime.aliveByHandle["worker-runtime"] = true
	release, ok := f.manager.AcquireSessionInput(source.ID)
	if !ok {
		t.Fatal("source input lease refused")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := f.manager.QuiesceReplacementSource(ctx, f.op); done <- err }()
	eventuallySessionInput(t, time.Second, func() bool { return f.manager.SessionMutationInProgress(source.ID) })
	workerRelease, ok := f.manager.AcquireSessionInput(worker.ID)
	if !ok {
		t.Fatal("source stop fenced worker input")
	}
	workerRelease()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) || f.runtime.destroyed != 0 {
		t.Fatalf("stop overtook admitted input: stops=%d err=%v", f.runtime.destroyed, err)
	}
	release()
	if _, err := f.manager.QuiesceReplacementSource(context.Background(), f.op); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.store.sessions[worker.ID], worker) || !f.runtime.aliveByHandle["worker-runtime"] || !reflect.DeepEqual(f.runtime.destroyedIDs, []string{"h1"}) {
		t.Fatal("source shutdown changed a worker")
	}
	bytes, err := os.ReadFile(file)
	if err != nil || string(bytes) != "preserve these bytes" || f.manager.workspace.(*fakeWorkspace).destroyed != 0 {
		t.Fatalf("source shutdown changed workspace: content=%q err=%v", bytes, err)
	}
}
