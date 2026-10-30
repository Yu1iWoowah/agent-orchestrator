package session

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type sharedSpawnGateKey struct{}

type sharedSpawnGateCommander struct {
	*fakeCommander
	gate    chan struct{}
	blocked chan struct{}
	entries atomic.Int32
}

func (m *sharedSpawnGateCommander) WithOrchestratorProjectGate(ctx context.Context, _ domain.ProjectID, fn func(context.Context) error) error {
	if ctx.Value(sharedSpawnGateKey{}) == m {
		return fn(ctx)
	}
	if m.entries.Add(1) == 2 {
		close(m.blocked)
	}
	select {
	case m.gate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-m.gate }()
	return fn(context.WithValue(ctx, sharedSpawnGateKey{}, m))
}

func (m *sharedSpawnGateCommander) Spawn(ctx context.Context, cfg ports.SpawnConfig) (rec domain.SessionRecord, prompt, system int, err error) {
	err = m.WithOrchestratorProjectGate(ctx, cfg.ProjectID, func(locked context.Context) error {
		var inner error
		rec, prompt, system, inner = m.fakeCommander.Spawn(locked, cfg)
		return inner
	})
	return
}

func TestOrchestratorSpawnEntrypointsShareAdmissionBeforeCheckingExisting(t *testing.T) {
	st := newFakeStore()
	st.projects["mer"] = domain.ProjectRecord{ID: "mer"}
	entered, release := make(chan struct{}), make(chan struct{})
	fc := &fakeCommander{}
	fc.spawnHook = func(context.Context) {
		if fc.spawnCalls == 0 {
			close(entered)
			<-release
		}
	}
	fc.spawnFunc = func(cfg ports.SpawnConfig) domain.SessionRecord {
		rec := domain.SessionRecord{ID: domain.SessionID(fmt.Sprintf("mer-%d", fc.spawnCalls)), ProjectID: cfg.ProjectID, Kind: cfg.Kind, Harness: domain.HarnessClaudeCode, CreatedAt: time.Now()}
		st.sessions[rec.ID] = rec
		return rec
	}
	manager := &sharedSpawnGateCommander{fakeCommander: fc, gate: make(chan struct{}, 1), blocked: make(chan struct{})}
	svc := &Service{manager: manager, store: st}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	type result struct {
		session domain.Session
		err     error
	}
	first, second := make(chan result, 1), make(chan result, 1)
	go func() { s, err := svc.SpawnOrchestrator(ctx, "mer", false, "", ""); first <- result{s, err} }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	go func() {
		s, _, _, err := svc.Spawn(ctx, ports.SpawnConfig{ProjectID: "mer", Kind: domain.KindOrchestrator})
		second <- result{s, err}
	}()
	select {
	case <-manager.blocked:
	case <-ctx.Done():
		close(release)
		t.Fatal(ctx.Err())
	}
	close(release)
	a, b := <-first, <-second
	if a.err != nil || b.err != nil {
		t.Fatalf("spawn errors: %v / %v", a.err, b.err)
	}
	if a.session.ID != b.session.ID || fc.spawnCalls != 1 || len(st.sessions) != 1 {
		t.Fatalf("parallel entrypoints created multiple orchestrators: first=%s second=%s spawns=%d rows=%d", a.session.ID, b.session.ID, fc.spawnCalls, len(st.sessions))
	}
}
