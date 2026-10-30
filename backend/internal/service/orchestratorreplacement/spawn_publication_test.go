package orchestratorreplacement_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestReplacementSpawnPublicationUsesCurrentAttemptAndRollsBack(t *testing.T) {
	x, op, seed := spawnAttemptFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()
	op, _, err := x.f.store.BeginReplacementSpawnAttempt(ctx, op.ID, seed.ID, "old-attempt", now)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := x.f.store.FailReplacementSpawnAttempt(ctx, op.ID, seed.ID, "old-attempt", "before launch", now); err != nil || !ok {
		t.Fatal(ok, err)
	}
	op = authorizeSpawnRetry(t, x, op.ID)
	op, _, err = x.f.store.BeginReplacementSpawnAttempt(ctx, op.ID, seed.ID, "current-attempt", now)
	if err != nil {
		t.Fatal(err)
	}
	publish := func(path string) func(context.Context) error {
		return func(reserved context.Context) error {
			if ok, err := x.f.store.SetSessionProvisionedWorkspace(reserved, seed.ID, "new-branch", path, "new-repo", now); err != nil || !ok {
				t.Fatal(ok, err)
			}
			return x.f.store.SetSessionProvisionSteps(reserved, seed.ID, []domain.SessionProvisionStep{{ID: domain.SessionProvisionStepAgent, Status: domain.SessionProvisionStepRunning}}, now)
		}
	}
	if err := x.f.store.WithReplacementSpawnPublication(ctx, op.ID, seed.ID, "current-attempt", publish("/current-workspace")); err != nil {
		t.Fatal(err)
	}
	before, _, err := x.f.store.GetSession(ctx, seed.ID)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	err = x.f.store.WithReplacementSpawnPublication(ctx, op.ID, seed.ID, "old-attempt", func(reserved context.Context) error { called = true; return publish("/obsolete-workspace")(reserved) })
	if err == nil || called {
		t.Fatal("stale publication entered callback", called, err)
	}
	sentinel := errors.New("publication ENOSPC")
	err = x.f.store.WithReplacementSpawnPublication(ctx, op.ID, seed.ID, "current-attempt", func(reserved context.Context) error {
		if err := publish("/partial-workspace")(reserved); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	after, _, err := x.f.store.GetSession(ctx, seed.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("stale/error publication left partial metadata", err)
	}
}
