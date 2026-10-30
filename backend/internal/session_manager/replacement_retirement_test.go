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
)

type retirementProof struct {
	op     domain.OrchestratorReplacement
	source domain.SessionRecord
	check  func(context.Context) error
}

func (p retirementProof) OperationID() string             { return p.op.ID }
func (p retirementProof) OperationRevision() int64        { return p.op.Revision }
func (p retirementProof) Source() domain.SessionRecord    { return p.source }
func (p retirementProof) DocumentRevision() int64         { return 42 }
func (p retirementProof) Check(ctx context.Context) error { return p.check(ctx) }

type retirementUsage struct {
	*fakeLCM
	calls int
	after func()
}

func (l *retirementUsage) FinalizeReplacementUsage(context.Context, domain.SessionRecord) error {
	l.calls++
	if l.after != nil {
		l.after()
	}
	return nil
}

type retirementStore struct {
	*replacementFenceStore
	commits   int
	commitErr error
}

func (s *retirementStore) CommitReplacementRetirement(ctx context.Context, id string, revision int64, source domain.SessionRecord, documentRevision int64, check func(context.Context) error) (domain.OrchestratorReplacement, bool, error) {
	s.commits++
	if id != s.operation.ID || revision != s.operation.Revision || documentRevision != 42 || source.ControllerOwner() != s.sessions[source.ID].ControllerOwner() || source.Metadata.BrowserCapabilityVerifier != s.sessions[source.ID].Metadata.BrowserCapabilityVerifier {
		return domain.OrchestratorReplacement{}, false, domain.ErrReplacementSourceChanged
	}
	if err := check(ctx); err != nil {
		return domain.OrchestratorReplacement{}, false, err
	}
	if s.commitErr != nil {
		return domain.OrchestratorReplacement{}, false, s.commitErr
	}
	rec := s.sessions[source.ID]
	rec.IsTerminated = true
	s.sessions[source.ID] = rec
	now := time.Now().UTC()
	s.operation.SourceRetiredAt = &now
	s.operation.Phase = domain.ReplacementStarting
	return s.operation, true, nil
}

func TestReplacementRetirementPreservesWorkspaceAndWorkers(t *testing.T) {
	for _, change := range []string{"current", "shell close fails", "commit ENOSPC", "live controller", "no stop receipt", "stale proof", "proof fails", "proof changes at commit", "owner changes during usage"} {
		t.Run(change, func(t *testing.T) {
			m, base, chat := replacementRuntimeManager(t)
			store := &retirementStore{replacementFenceStore: base}
			m.store = store
			usage := &retirementUsage{fakeLCM: m.lcm.(*fakeLCM)}
			m.lcm = usage
			source := base.sessions[base.operation.SourceSessionID]
			source.Metadata.WorkspacePath = t.TempDir()
			source.Metadata.Branch = "preserve-dirty-branch"
			now := time.Now().UTC()
			source.HibernatedAt = &now
			base.sessions[source.ID] = source
			base.worktrees[source.ID] = []domain.SessionWorktreeRecord{{SessionID: source.ID, WorktreePath: source.Metadata.WorkspacePath, PreservedRef: "existing-marker"}}
			path := filepath.Join(source.Metadata.WorkspacePath, "untracked.txt")
			if err := os.WriteFile(path, []byte("preserve source bytes"), 0o600); err != nil {
				t.Fatal(err)
			}
			worker := mkLive("mer-worker")
			worker.Kind, worker.Harness = domain.KindWorker, domain.HarnessCodex
			base.sessions[worker.ID] = worker
			base.operation.Phase = domain.ReplacementRetiring
			base.operation.SourceStop = &domain.ReplacementControllerStop{Owner: source.ControllerOwner(), CapabilityVerifier: source.Metadata.BrowserCapabilityVerifier, NativeID: source.Metadata.ProviderConversationID, Method: "chat-hibernate", ConfirmedAt: now}
			proofChecks := 0
			proof := retirementProof{op: base.operation, source: source, check: func(ctx context.Context) error {
				proofChecks++
				if change == "proof fails" || change == "proof changes at commit" && proofChecks >= 2 {
					return errors.New("evidence changed")
				}
				return ctx.Err()
			}}
			closer := &fakeShellTerminalCloser{}
			m.SetShellTerminalCloser(closer)
			switch change {
			case "shell close fails":
				closer.err = errors.New("shell still alive")
			case "commit ENOSPC":
				store.commitErr = errors.New("ENOSPC")
			case "live controller":
				chat.live = true
			case "no stop receipt":
				base.operation.SourceStop = nil
			case "stale proof":
				proof.op.Revision--
			case "owner changes during usage":
				usage.after = func() {
					rec := base.sessions[source.ID]
					rec.Metadata.ControllerGeneration = "rotated"
					base.sessions[source.ID] = rec
				}
			}
			result, err := m.RetireReplacementSource(context.Background(), base.operation, proof)
			if change == "current" || change == "shell close fails" {
				if (err != nil) != (change == "shell close fails") || result.SourceRetiredAt == nil || result.Phase != domain.ReplacementStarting || !base.sessions[source.ID].IsTerminated || usage.calls != 1 || store.commits != 1 {
					t.Fatalf("retirement not committed: phase=%s usage=%d commits=%d err=%v", result.Phase, usage.calls, store.commits, err)
				}
			} else if err == nil || base.sessions[source.ID].IsTerminated {
				t.Fatalf("%s retired without current proof: %v", change, err)
			}
			if change != "current" && change != "shell close fails" && len(closer.began) != 0 {
				t.Fatal("retirement that did not commit closed source shell", change)
			}
			if change == "no stop receipt" || change == "stale proof" || change == "live controller" || change == "proof fails" {
				if len(closer.began) != 0 || usage.calls != 0 || store.commits != 0 {
					t.Fatal("unverified source reached teardown")
				}
			}
			if len(closer.began) > 0 && change != "shell close fails" && !reflect.DeepEqual(closer.began, closer.ended) {
				t.Fatal("source shell gate leaked")
			}
			if len(closer.began) > 0 && closer.began[0] != source.ID {
				t.Fatal("closed worker shell")
			}
			if m.runtime.(*fakeRuntime).destroyed != 0 || m.workspace.(*fakeWorkspace).destroyed != 0 || len(base.worktrees[source.ID]) != 1 || !reflect.DeepEqual(base.sessions[worker.ID], worker) {
				t.Fatal("retirement changed workspace, runtime or worker")
			}
			data, readErr := os.ReadFile(path)
			if readErr != nil || string(data) != "preserve source bytes" {
				t.Fatal("source file lost", readErr)
			}
		})
	}
}

func TestReplacementRetirementTUIRechecksOriginalDeadLaunch(t *testing.T) {
	for _, change := range []string{"dead", "alive again", "unknown"} {
		t.Run(change, func(t *testing.T) {
			f := newReplacementTUIFixture(t)
			f.acceptControl()
			f.writeNative(t, true, false, "")
			now := time.Now().UTC()
			f.op.Phase, f.op.FrozenAt, f.op.SourceStopIntentAt = domain.ReplacementExportingTranscript, &now, &now
			f.op.Preparation.VerifiedAt = &now
			f.op.Transcript = &domain.ReplacementTranscript{AttemptID: "evidence"}
			f.store.operation = f.op
			stop, err := f.manager.QuiesceReplacementSource(context.Background(), f.op)
			if err != nil {
				t.Fatal(err)
			}
			store := &retirementStore{replacementFenceStore: f.store.replacementFenceStore}
			store.operation.Phase, store.operation.SourceStop = domain.ReplacementRetiring, &stop
			f.manager.store = store
			usage := &retirementUsage{fakeLCM: f.manager.lcm.(*fakeLCM)}
			f.manager.lcm = usage
			source := store.sessions[f.op.SourceSessionID]
			proof := retirementProof{op: store.operation, source: source, check: func(ctx context.Context) error { return ctx.Err() }}
			closer := &fakeShellTerminalCloser{}
			f.manager.SetShellTerminalCloser(closer)
			stops := f.runtime.destroyed
			if change == "alive again" {
				f.runtime.aliveByHandle[source.Metadata.RuntimeHandleID] = true
			}
			if change == "unknown" {
				f.runtime.aliveErr = errors.New("unavailable runtime probe")
			}
			_, err = f.manager.RetireReplacementSource(context.Background(), store.operation, proof)
			if (err == nil) != (change == "dead") {
				t.Fatalf("%s: %v", change, err)
			}
			if f.runtime.destroyed != stops {
				t.Fatal("retirement repeated a provider stop")
			}
			if change != "dead" && (usage.calls != 0 || len(closer.began) != 0 || store.sessions[source.ID].IsTerminated) {
				t.Fatal("unconfirmed source reached retirement effects")
			}
		})
	}
}
