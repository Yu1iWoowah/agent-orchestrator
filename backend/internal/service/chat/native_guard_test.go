package chat

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/registry"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type nativeCountingDriver struct{ calls atomic.Int32 }

func (*nativeCountingDriver) Harness() domain.AgentHarness                          { return domain.HarnessCodex }
func (*nativeCountingDriver) Probe(context.Context) (ports.ChatCapabilities, error) { return nil, nil }
func (d *nativeCountingDriver) Start(context.Context, ports.ChatStartConfig) (ports.ChatConversation, error) {
	d.calls.Add(1)
	return nil, nil
}
func (d *nativeCountingDriver) Resume(context.Context, ports.ChatResumeConfig) (ports.ChatConversation, error) {
	d.calls.Add(1)
	return nil, nil
}
func (d *nativeCountingDriver) Reconnect(context.Context, ports.ChatResumeConfig) (ports.ChatConversation, error) {
	d.calls.Add(1)
	return nil, nil
}
func (d *nativeCountingDriver) ForkIntoHost(context.Context, string, string, ports.ChatStartConfig) (ports.ChatConversation, error) {
	d.calls.Add(1)
	return nil, nil
}

func TestNativeProtectedDriverBoundary(t *testing.T) {
	for _, provider := range []domain.AgentHarness{domain.HarnessCodex, domain.HarnessClaudeCode} {
		t.Run(string(provider), func(t *testing.T) {
			svc, db, _, home := newRecordHarness(t, provider)
			_, _ = svc.CompactSideChat(t.Context(), "session", "side")
			// Alias and real root must identify the same native conversation.
			alias := filepath.Join(t.TempDir(), "alias")
			if err := os.Symlink(home, alias); err != nil {
				t.Fatal(err)
			}
			cfg := ports.ChatResumeConfig{Env: map[string]string{"CODEX_HOME": alias, "CLAUDE_CONFIG_DIR": alias}, ProviderConversationID: "provider-side-1", SessionID: "another-host"}
			counter := &nativeCountingDriver{}
			original := &nativeProviderDriver{nativeCountingDriver: counter, provider: provider}
			reg := registry.New(original)
			reg.SetNativeGuard(svc.nativeGuard)
			driver, _ := reg.Driver(provider)
			cases := []struct {
				name string
				run  func() error
			}{
				{"other-session-resume", func() error { _, err := driver.Resume(t.Context(), cfg); return err }},
				{"other-side-reconnect", func() error { _, err := driver.(ports.ChatDriverReconnector).Reconnect(t.Context(), cfg); return err }},
				{"new-host-fork", func() error {
					_, err := driver.(ports.ChatIsolatedForker).ForkIntoHost(t.Context(), cfg.ProviderConversationID, "", ports.ChatStartConfig{Env: cfg.Env})
					return err
				}},
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					if err := tc.run(); !errors.Is(err, ErrNativeConversationProtected) {
						t.Fatal(err)
					}
					if counter.calls.Load() != 0 {
						t.Fatal("protected native I/O", counter.calls.Load())
					}
				})
			}
			cfg.ProviderConversationID = "different-thread"
			if _, err := driver.Resume(t.Context(), cfg); err != nil {
				t.Fatal(err)
			}
			if _, err := driver.Start(t.Context(), ports.ChatStartConfig{}); err != nil {
				t.Fatal(err)
			}
			if counter.calls.Load() != 2 {
				t.Fatal(counter.calls.Load())
			}
			if err := svc.nativeGuard.DeleteHome(t.Context(), alias, func() error { t.Fatal("deleted protected home"); return nil }); !errors.Is(err, ErrNativeConversationProtected) {
				t.Fatal(err)
			}
			if _, err := os.Stat(home); err != nil {
				t.Fatal(err)
			}
			rows, _ := db.SideCompactions(t.Context(), true)
			if len(rows) != 1 {
				t.Fatal(rows)
			}
			if err := db.TransitionSideCompaction(t.Context(), rows[0].ID, rows[0].Epoch, rows[0].State, false, true); err != nil {
				t.Fatal(err)
			}
			deleted := false
			if err := svc.nativeGuard.DeleteHome(t.Context(), home, func() error { deleted = true; return nil }); err != nil || !deleted {
				t.Fatal(err, deleted)
			}
		})
	}
}

type nativeProviderDriver struct {
	*nativeCountingDriver
	provider domain.AgentHarness
}

func (d *nativeProviderDriver) Harness() domain.AgentHarness { return d.provider }

func TestNativeRegistrationAndResumeSerialized(t *testing.T) {
	svc, db, _, home := newRecordHarness(t, domain.HarnessCodex)
	entered, release := make(chan struct{}), make(chan struct{})
	cfg := ports.ChatResumeConfig{ProviderConversationID: "provider-side-1", Env: map[string]string{"CODEX_HOME": home}}
	first := make(chan error, 1)
	go func() {
		first <- svc.nativeGuard.Run(t.Context(), domain.HarnessCodex, cfg, func() error { close(entered); <-release; return nil })
	}()
	<-entered
	registered := make(chan error, 1)
	runtime := svc.sides.runtimes["side"]
	record, err := svc.sides.prepareCompaction(t.Context(), runtime)
	if err != nil {
		t.Fatal(err)
	}
	record.ID, record.Epoch = "register-race", "epoch"
	go func() { registered <- svc.sides.registerCompaction(t.Context(), record) }()
	// An unrelated thread must progress while the first key is held.
	different := cfg
	different.ProviderConversationID = "other"
	if err := svc.nativeGuard.Run(t.Context(), domain.HarnessCodex, different, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-registered:
		t.Fatal("registration passed active native operation", err)
	case <-time.After(20 * time.Millisecond):
	}
	// The native guard never owns the side/controller mutexes.
	svc.sides.mu.Lock()
	runtime.mu.Lock()
	sideID := runtime.side.ID
	runtime.mu.Unlock()
	svc.sides.mu.Unlock()
	if sideID != "side" {
		t.Fatal(sideID)
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-registered; err != nil {
		t.Fatal(err)
	}
	called := false
	if err := svc.nativeGuard.Run(t.Context(), domain.HarnessCodex, cfg, func() error { called = true; return nil }); !errors.Is(err, ErrNativeConversationProtected) || called {
		t.Fatal(err, called)
	}
	rows, _ := db.SideCompactions(t.Context(), true)
	if len(rows) != 1 {
		t.Fatal(rows)
	}
}

func TestNativeOriginalReconnectException(t *testing.T) {
	svc, db, _, home := newRecordHarness(t, domain.HarnessCodex)
	_, _ = svc.CompactSideChat(t.Context(), "session", "side")
	rows, _ := db.SideCompactions(t.Context(), true)
	row := rows[0]
	cfg := ports.ChatResumeConfig{ReconnectOnly: true, ProviderConversationID: row.NativeID, SessionID: domain.SessionID(row.ProviderHostID), Env: map[string]string{"CODEX_HOME": home}}
	ctx := ports.WithChatCompactionEpoch(ports.WithChatCompactionID(t.Context(), row.ID), row.Epoch)
	calls := 0
	call := func() error { calls++; return nil }
	if err := svc.nativeGuard.Run(ctx, row.Provider, cfg, call); err != nil || calls != 1 {
		t.Fatal(err, calls)
	}
	if err := svc.nativeGuard.Run(t.Context(), row.Provider, cfg, call); !errors.Is(err, ErrNativeConversationProtected) {
		t.Fatal(err)
	}
	// Repointing an invocation alias cannot change the identity of a live host.
	cfg.Env = map[string]string{"CODEX_HOME": t.TempDir()}
	if err := svc.nativeGuard.Run(t.Context(), row.Provider, cfg, call); !errors.Is(err, ErrNativeConversationProtected) {
		t.Fatal("host canonical root changed", err)
	}

	if err := db.TransitionSideCompaction(t.Context(), row.ID, row.Epoch, domain.SideCompactionUnknown, true, false); err != nil {
		t.Fatal(err)
	}
	if err := svc.nativeGuard.Run(ctx, row.Provider, cfg, call); !errors.Is(err, ErrNativeConversationProtected) || calls != 1 {
		t.Fatal(err, calls)
	}
}

func TestNativeGuardInheritedProviderHome(t *testing.T) {
	for _, tc := range []struct {
		provider domain.AgentHarness
		variable string
	}{
		{domain.HarnessCodex, "CODEX_HOME"},
		{domain.HarnessClaudeCode, "CLAUDE_CONFIG_DIR"},
		{domain.HarnessOpenCodeV2, "XDG_DATA_HOME"},
	} {
		t.Run(string(tc.provider), func(t *testing.T) {
			svc, db, _, _ := newRecordHarness(t, tc.provider)
			home := t.TempDir()
			t.Setenv("HOME", t.TempDir())
			t.Setenv(tc.variable, home)
			if tc.provider == domain.HarnessOpenCodeV2 {
				home = filepath.Join(home, "opencode-v2-home", "opencode")
				if err := os.MkdirAll(home, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.RegisterSideCompaction(t.Context(), domain.SideCompaction{ID: "inherited", SideID: "protected", NativeHome: home, NativeID: "thread", Provider: tc.provider, State: domain.SideCompactionPending, Epoch: "epoch", Content: json.RawMessage(`{}`)}); err != nil {
				t.Fatal(err)
			}
			counter := &nativeCountingDriver{}
			reg := registry.New(&nativeProviderDriver{nativeCountingDriver: counter, provider: tc.provider})
			reg.SetNativeGuard(svc.nativeGuard)
			driver, _ := reg.Driver(tc.provider)
			env := map[string]string{}
			if tc.provider == domain.HarnessOpenCodeV2 {
				// The launch binding replaces this session override with its daemon-derived root.
				env["XDG_DATA_HOME"] = t.TempDir()
			}
			cfg := ports.ChatResumeConfig{SessionID: "another-host", ProviderConversationID: "thread", Env: env}
			for _, op := range []struct {
				name string
				call func() error
			}{
				{"resume", func() error { _, err := driver.Resume(t.Context(), cfg); return err }},
				{"reconnect", func() error { _, err := driver.(ports.ChatDriverReconnector).Reconnect(t.Context(), cfg); return err }},
				{"fork", func() error {
					_, err := driver.(ports.ChatIsolatedForker).ForkIntoHost(t.Context(), "thread", "", ports.ChatStartConfig{Env: env})
					return err
				}},
			} {
				t.Run(op.name, func(t *testing.T) {
					err := op.call()
					if !errors.Is(err, ErrNativeConversationProtected) || counter.calls.Load() != 0 {
						t.Fatalf("inherited native home was not protected: error=%v, provider I/O=%d", err, counter.calls.Load())
					}
				})
			}
		})
	}
}

func TestNativeGuardUnavailableHomeHasDistinctError(t *testing.T) {
	for _, tc := range []struct {
		name, rowID string
	}{
		{"no-protected-row", ""},
		{"unrelated-protected-thread", "other-thread"},
		{"possibly-protected-thread", "thread"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, db, _, _ := newRecordHarness(t, domain.HarnessCodex)
			file := filepath.Join(t.TempDir(), "file")
			if err := os.WriteFile(file, []byte("not a directory"), 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("CODEX_HOME", filepath.Join(file, "native"))
			if tc.rowID != "" {
				if err := db.RegisterSideCompaction(t.Context(), domain.SideCompaction{ID: "unknown-home", SideID: "side", NativeHome: t.TempDir(), NativeID: tc.rowID, State: domain.SideCompactionPending, Content: json.RawMessage(`{}`)}); err != nil {
					t.Fatal(err)
				}
			}
			counter := &nativeCountingDriver{}
			reg := registry.New(counter)
			reg.SetNativeGuard(svc.nativeGuard)
			driver, _ := reg.Driver(domain.HarnessCodex)
			_, err := driver.Resume(t.Context(), ports.ChatResumeConfig{ProviderConversationID: "thread"})
			if !errors.Is(err, ErrNativeHomeUnavailable) || errors.Is(err, ErrNativeConversationProtected) || counter.calls.Load() != 0 {
				t.Fatal("wrong unavailable-home classification", err, counter.calls.Load())
			}
		})
	}
}
