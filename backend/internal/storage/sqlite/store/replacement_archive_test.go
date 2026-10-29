package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	fixturestore "github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

func TestReplacementArchivedProjectCannotStartOrPublishOrRetry(t *testing.T) {
	for _, action := range []string{"create", "publication", "retry"} {
		t.Run(action, func(t *testing.T) {
			st := newTestStore(t)
			ctx := context.Background()
			op := replacementOperation(t, st)
			project, _, err := st.GetProject(ctx, string(op.ProjectID))
			if err != nil {
				t.Fatal(err)
			}
			op.ProjectFingerprint, err = domain.ReplacementProjectFingerprint(project)
			if err != nil {
				t.Fatal(err)
			}
			var source domain.SessionRecord
			var document domain.ProjectDocumentState
			if action != "create" {
				op, _, err = st.CreateOrchestratorReplacement(ctx, op)
				if err != nil {
					t.Fatal(err)
				}
				if action == "publication" {
					op.Phase = domain.ReplacementPreparingHandoff
					op = fixturestore.SeedReplacementTestFacts(t, st, op)
					source, _, err = st.GetSession(ctx, op.SourceSessionID)
					if err != nil {
						t.Fatal(err)
					}
					document = domain.ProjectDocumentState{Lease: &domain.ProjectDocumentLease{SessionID: source.ID, Owner: source.ControllerOwner(), Epoch: 7, TokenHash: "token-hash"}}
					if saved, err := st.SaveProjectDocumentState(ctx, op.ProjectID, document); err != nil || !saved {
						t.Fatal(saved, err)
					}
					document, err = st.GetProjectDocumentState(ctx, op.ProjectID)
					if err != nil {
						t.Fatal(err)
					}
				} else {
					op.Phase = domain.ReplacementAwaitingSource
					if saved, err := st.SaveOrchestratorReplacement(ctx, op); err != nil || !saved {
						t.Fatal(saved, err)
					}
					op, _, err = st.GetOrchestratorReplacement(ctx, op.ID)
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			if archived, err := st.ArchiveProject(ctx, string(op.ProjectID), time.Now()); err != nil || !archived {
				t.Fatal(archived, err)
			}
			switch action {
			case "create":
				if _, created, err := st.CreateOrchestratorReplacement(ctx, op); created || !errors.Is(err, domain.ErrReplacementConfigChanged) {
					t.Fatalf("archived project admitted operation: created=%t err=%v", created, err)
				}
			case "publication":
				called := false
				published, err := st.WithReplacementDocumentPublication(ctx, op.ID, op.Revision, source, document.Revision, func(context.Context) error { called = true; return nil })
				if err != nil || published || called {
					t.Fatalf("archived project published: published=%t called=%t err=%v", published, called, err)
				}
			case "retry":
				if _, applied, err := st.ResetReplacementBeforeStop(ctx, op.ID, op.Revision, false, time.Now(), time.Now().Add(time.Hour)); applied || err != nil {
					t.Fatalf("archived project retried: applied=%t err=%v", applied, err)
				}
			}
		})
	}
}

func TestReplacementArchivedProjectRefusesFreshLateInput(t *testing.T) {
	for _, released := range []bool{false, true} {
		t.Run(map[bool]string{false: "before successor", true: "released"}[released], func(t *testing.T) {
			f := newCanonicalIntakeFixture(t, false, released)
			ctx := context.Background()
			if released {
				if _, applied, err := f.api.ReleaseReplacementIntake(ctx, f.op.ID, f.op.Revision, time.Now()); err != nil || !applied {
					t.Fatal(applied, err)
				}
			}
			svc := lateIntakeChat(f.st, f.st)
			priorMessage := ports.ChatUserMessage{ClientMessageID: "before-archive", Text: "accepted input", Origin: domain.MessageOriginHuman}
			prior, err := svc.Send(ctx, f.op.SourceSessionID, priorMessage)
			if err != nil {
				t.Fatal(err)
			}
			if archived, err := f.st.ArchiveProject(ctx, string(f.op.ProjectID), time.Now()); err != nil || !archived {
				t.Fatal(archived, err)
			}
			if replay, err := svc.Send(ctx, f.op.SourceSessionID, priorMessage); err != nil || replay.ID != prior.ID {
				t.Fatal("archive invalidated committed receipt", replay.ID, err)
			}
			_, err = svc.Send(ctx, f.op.SourceSessionID, ports.ChatUserMessage{ClientMessageID: "after-archive", Text: "new input", Origin: domain.MessageOriginHuman})
			if !errors.Is(err, domain.ErrReplacementIntakeFenced) {
				t.Fatalf("archived project accepted fresh late input: %v", err)
			}
		})
	}
}
