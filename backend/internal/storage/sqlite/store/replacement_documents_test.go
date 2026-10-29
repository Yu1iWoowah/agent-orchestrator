package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestReplacementDocumentPublicationIsAtomicAndFenced(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	op, _, err := st.CreateOrchestratorReplacement(ctx, replacementOperation(t, st))
	if err != nil {
		t.Fatal(err)
	}
	op.Phase = domain.ReplacementPreparingHandoff
	if ok, err := st.SaveOrchestratorReplacement(ctx, op); err != nil || !ok {
		t.Fatal(ok, err)
	}
	op, _, err = st.GetOrchestratorReplacement(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	source, _, err := st.GetSession(ctx, op.SourceSessionID)
	if err != nil {
		t.Fatal(err)
	}
	state := domain.ProjectDocumentState{Lease: &domain.ProjectDocumentLease{SessionID: source.ID, Owner: source.ControllerOwner(), Epoch: 7, TokenHash: "not-a-raw-token"}}
	if ok, err := st.SaveProjectDocumentState(ctx, op.ProjectID, state); err != nil || !ok {
		t.Fatal(ok, err)
	}
	state, err = st.GetProjectDocumentState(ctx, op.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	injected := errors.New("crash before commit")
	called := false
	publish := func(reserved context.Context) error {
		called = true
		current, err := st.GetProjectDocumentState(reserved, op.ProjectID)
		if err != nil {
			return err
		}
		current.Lease.Frozen = true
		if ok, err := st.SaveProjectDocumentState(reserved, op.ProjectID, current); err != nil {
			return err
		} else if !ok {
			return errors.New("lease CAS lost inside reservation")
		}
		before, found, err := st.GetOrchestratorReplacement(reserved, op.ID)
		if err != nil {
			return err
		} else if !found {
			return errors.New("operation disappeared")
		}
		before.Phase = domain.ReplacementHandoffVerified
		if ok, err := st.SaveOrchestratorReplacement(reserved, before); err != nil {
			return err
		} else if !ok {
			return errors.New("operation CAS lost inside reservation")
		}
		return nil
	}
	ok, err := st.WithReplacementDocumentPublication(ctx, op.ID, op.Revision, source, state.Revision, func(tx context.Context) error {
		if err := publish(tx); err != nil {
			return err
		}
		return injected
	})
	if ok || !errors.Is(err, injected) || !called {
		t.Fatalf("publication callback not run/rolled back: %t %t %v", ok, called, err)
	}
	check, _, err := st.GetOrchestratorReplacement(ctx, op.ID)
	if err != nil || check.Phase != op.Phase || check.Revision != op.Revision {
		t.Fatal("operation escaped rollback", err)
	}
	doc, err := st.GetProjectDocumentState(ctx, op.ProjectID)
	if err != nil || doc.Lease.Frozen || doc.Revision != state.Revision {
		t.Fatal("lease escaped rollback", err)
	}
	ok, err = st.WithReplacementDocumentPublication(ctx, op.ID, op.Revision, source, state.Revision, publish)
	if err != nil || !ok {
		t.Fatal("atomic publication refused", ok, err)
	}
	called = false
	if ok, err = st.WithReplacementDocumentPublication(ctx, op.ID, op.Revision, source, state.Revision, func(context.Context) error { called = true; return nil }); err != nil || ok || called {
		t.Fatal("stale operation/lease published", ok, called, err)
	}
	check, _, err = st.GetOrchestratorReplacement(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	doc, err = st.GetProjectDocumentState(ctx, op.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	source.Metadata.BrowserCapabilityVerifier = "rotated-verifier"
	if err := st.UpdateSession(ctx, source); err != nil {
		t.Fatal(err)
	}
	if ok, err = st.WithReplacementDocumentPublication(ctx, op.ID, check.Revision, source, doc.Revision, func(context.Context) error { called = true; return nil }); err != nil || ok || called {
		t.Fatal("rotated source published against old reservation", ok, called, err)
	}
}
