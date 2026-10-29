package shellterm

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestTrustedTerminalIntentSurvivesCrashDuringCreate(t *testing.T) {
	ctx := context.Background()
	rt := newFakeShellRuntime()
	st := &fakeShellTerminalStore{}
	svc := newTestService(rt, st, &fakeProjectRootLocator{})
	svc.dataDir = t.TempDir()
	crash := errors.New("simulated daemon crash")
	rt.onCreate = func(cfg ports.RuntimeConfig) {
		found := false
		for _, row := range st.records {
			if row.HandleID == string(cfg.SessionID) && row.Transient {
				found = true
			}
		}
		if !found {
			t.Fatal("trusted runtime starts without durable recovery intent")
		}
		rt.aliveByHandle[string(cfg.SessionID)] = true
		panic(crash)
	}
	func() {
		defer func() {
			if r, ok := recover().(error); !ok || !errors.Is(r, crash) {
				t.Fatalf("crash=%v", r)
			}
		}()
		_, _ = svc.OpenCommandTerminal(ctx, OpenCommandTerminalInput{Argv: []string{"fake-login"}, Title: "Fake login"})
	}()
	restarted := newTestService(rt, st, &fakeProjectRootLocator{})
	restarted.dataDir = svc.dataDir
	restarted.appRunID = "next-run"
	if _, err := restarted.ReapShellTerminalsFromPreviousAppRuns(ctx); err != nil {
		t.Fatal(err)
	}
	if len(st.records) != 0 || len(rt.aliveByHandle) != 0 {
		t.Fatal("orphaned trusted terminal survives recovery")
	}
}

func TestTrustedTerminalDoesNotSpawnWithoutDurableIntent(t *testing.T) {
	rt := newFakeShellRuntime()
	st := &fakeShellTerminalStore{insertErr: errors.New("disk full")}
	svc := newTestService(rt, st, &fakeProjectRootLocator{})
	svc.dataDir = t.TempDir()
	_, err := svc.OpenCommandTerminal(context.Background(), OpenCommandTerminalInput{Argv: []string{"fake-login"}, Title: "Fake login"})
	if err == nil || len(rt.created) != 0 {
		t.Fatal("trusted terminal spawned despite failed durable intent")
	}
}

func TestListCannotForgetTrustedIntentWhileCreateIsInFlight(t *testing.T) {
	ctx := context.Background()
	rt := newFakeShellRuntime()
	st := &fakeShellTerminalStore{}
	svc := newTestService(rt, st, &fakeProjectRootLocator{})
	svc.dataDir = t.TempDir()
	entered, release := make(chan struct{}), make(chan struct{})
	rt.onCreate = func(ports.RuntimeConfig) { close(entered); <-release }
	opened := make(chan error, 1)
	go func() {
		_, e := svc.OpenCommandTerminal(ctx, OpenCommandTerminalInput{Argv: []string{"fake-login"}, Title: "Fake login"})
		opened <- e
	}()
	<-entered
	listed := make(chan error, 1)
	go func() { _, e := svc.ListShellTerminalsForCurrentAppRun(ctx); listed <- e }()
	// Reconciliation must wait at the creation boundary, rather than observing
	// an absent runtime and deleting its durable ownership record.
	select {
	case e := <-listed:
		close(release)
		<-opened
		t.Fatalf("list crossed in-flight trusted create: %v", e)
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	if e := <-opened; e != nil {
		t.Fatal(e)
	}
	if e := <-listed; e != nil {
		t.Fatal(e)
	}
	if len(st.records) != 1 || len(rt.aliveByHandle) != 1 {
		t.Fatal("live trusted child lost durable ownership")
	}
}
