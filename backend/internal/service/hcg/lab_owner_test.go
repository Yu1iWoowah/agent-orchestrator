package hcg

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/datadirlock"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/components/hcg/reporting"
)

func TestLabOwnerRequiresActualRootAndCanonicalIdentity(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	lock, err := datadirlock.Acquire(t.Context(), root, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	if second, err := datadirlock.Acquire(t.Context(), root, 0); !errors.Is(err, datadirlock.ErrLocked) {
		if second != nil {
			second.Release()
		}
		t.Fatalf("second canonical owner acquired root: %v", err)
	}
	store, err := sqlite.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.UpsertProject(t.Context(), domain.ProjectRecord{ID: "synthetic-project", Path: root}); err != nil {
		t.Fatal(err)
	}
	rec, err := store.CreateSession(t.Context(), domain.SessionRecord{ProjectID: "synthetic-project", Kind: domain.KindWorker, Harness: domain.HarnessCodex, Mode: domain.SessionModeChat, CreatedAt: time.Now(), UpdatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	owner := reporting.DelegationLabSession{ProjectID: "synthetic-project", SessionID: string(rec.ID)}
	g := &reporting.DelegationLabGrant{OwnerRoot: root, CanonicalOwner: owner, Bindings: []reporting.DelegationLabSession{owner}}
	svc := &Service{root: root, store: store}
	if err := svc.validateLabOwner(t.Context(), g); err != nil {
		t.Fatalf("original owner rejected: %v", err)
	}
	wrongRoot := *g
	wrongRoot.OwnerRoot = t.TempDir()
	if svc.validateLabOwner(t.Context(), &wrongRoot) == nil {
		t.Fatal("another root accepted")
	}
	missing := *g
	missing.Bindings = []reporting.DelegationLabSession{{ProjectID: owner.ProjectID, SessionID: "missing-original-session"}}
	if svc.validateLabOwner(t.Context(), &missing) == nil {
		t.Fatal("missing canonical identity accepted")
	}
	svc.root = t.TempDir()
	wrongRoot.OwnerRoot = svc.root
	if svc.validateLabOwner(t.Context(), &wrongRoot) == nil {
		t.Fatal("service claim replaced actual DB root")
	}
}
