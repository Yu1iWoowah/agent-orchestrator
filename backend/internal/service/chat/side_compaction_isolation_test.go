package chat

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/registry"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type isolationConversation struct {
	*recordCompactionConversation
	closes, interrupts, deletes int
}

func (c *isolationConversation) Close() error                             { c.closes++; return nil }
func (c *isolationConversation) Interrupt(context.Context, string) error  { c.interrupts++; return nil }
func (c *isolationConversation) DeleteFork(context.Context, string) error { c.deletes++; return nil }

func TestProtectedSideAutomaticPaths(t *testing.T) {
	cases := []struct {
		name string
		run  func(*Service, domain.SideConversation) error
	}{
		{"quit-retire", func(s *Service, _ domain.SideConversation) error { return s.RetireSideChatLaunch(t.Context(), "run") }},
		{"initialize", func(s *Service, _ domain.SideConversation) error { return s.InitializeSideChats(t.Context()) }},
		{"claim", func(s *Service, _ domain.SideConversation) error { return s.ClaimSideChatLaunch(t.Context(), "run") }},
		{"snapshot", func(s *Service, _ domain.SideConversation) error {
			_, err := s.SideSnapshot(t.Context(), "session", "side", time.Time{}, 0)
			return err
		}},
		{"retry", func(s *Service, _ domain.SideConversation) error {
			return s.RetrySideConnection(t.Context(), "session", "side")
		}},
		{"recover-untrusted-projection", func(s *Service, side domain.SideConversation) error {
			return s.RecoverSideChatLaunch(t.Context(), "run", []SideRecoveryRecord{{Side: side, CleanupOnly: true}})
		}},
		{"background-open", func(s *Service, side domain.SideConversation) error {
			s.sides.open(side, StartConfig{}, nil)
			return nil
		}},
		{"cold-restore", func(s *Service, side domain.SideConversation) error { s.sides.restore(t.Context(), side); return nil }},
		{"deferred-delete", func(s *Service, side domain.SideConversation) error {
			s.sides.deleteRegisteredFork(t.Context(), side, side.ProviderForkID)
			return nil
		}},
		{"batch-cleanup", func(s *Service, _ domain.SideConversation) error { s.sides.retryCleanup(); return nil }},
		{"close-session-sides", func(s *Service, _ domain.SideConversation) error { return s.closeSessionSides(t.Context(), "session") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, db, conv, _ := newRecordHarness(t, domain.HarnessCodex)
			_, _ = svc.CompactSideChat(t.Context(), "session", "side")
			before, _ := db.SideCompactions(t.Context(), true)
			spy := &isolationConversation{recordCompactionConversation: conv}
			runtime := svc.sides.runtimes["side"]
			runtime.conv = spy
			stops := 0
			svc.stopProviderHost = func(context.Context, domain.SessionID) error { stops++; return nil }
			store := svc.sides.store.(*memorySideStore)
			side, _ := store.SideConversation(t.Context(), "side")
			if err := tc.run(svc, side); err != nil {
				t.Fatal(err)
			}
			after, _ := db.SideCompactions(t.Context(), true)
			if len(after) != 1 || after[0].Closed || string(before[0].Content) != string(after[0].Content) {
				t.Fatal("automatic path changed obligation/content", after)
			}
			if stops+spy.closes+spy.interrupts+spy.deletes != 0 {
				t.Fatal("automatic provider I/O", stops, spy.closes, spy.interrupts, spy.deletes)
			}
			if _, err := store.SideConversation(t.Context(), "side"); err != nil {
				t.Fatal("lost side projection", err)
			}
			svc.sides.cancel()
		})
	}
}

func TestProtectedSideStopCallersShareDefault(t *testing.T) {
	// All these owner operations enter Service.Stop -> closeSessionSides. Keep
	// their observable shared boundary under test rather than fake each manager.
	for _, name := range []string{"ui-kill", "orchestrator-kill", "terminate-restore", "chat-to-tui", "account-switch", "exit-agents-on-account", "stop-for-owner", "spawn-rollback", "restore-rollback", "exit-agent", "choose-start-settings"} {
		t.Run(name, func(t *testing.T) {
			svc, db, conv, _ := newRecordHarness(t, domain.HarnessCodex)
			_, _ = svc.CompactSideChat(t.Context(), "session", "side")
			spy := &isolationConversation{recordCompactionConversation: conv}
			svc.sides.runtimes["side"].conv = spy
			svc.stopProviderHost = func(_ context.Context, id domain.SessionID) error {
				if id != "session" {
					t.Fatal("stopped protected host", id)
				}
				return nil
			}
			stop := func() error { return svc.Stop(t.Context(), "session") }
			if name == "stop-for-owner" {
				stop = func() error { return svc.StopForOwner(t.Context(), domain.SessionConversationOwner("session")) }
			}
			if err := stop(); err != nil {
				t.Fatal(err)
			}
			rows, _ := db.SideCompactions(t.Context(), true)
			if len(rows) != 1 || rows[0].Closed || spy.closes != 0 || spy.deletes != 0 {
				t.Fatal("stop destroyed protected side", rows, spy)
			}
			if svc.sides.runtimes["side"] == nil {
				t.Fatal("stop removed protected runtime")
			}
		})
	}
}

func TestProtectedSideGlobalListAndClose(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(map[bool]string{false: "old-launch", true: "corrupt-content"}[corrupt], func(t *testing.T) {
			svc, db, _, _ := newRecordHarness(t, domain.HarnessCodex)
			_, _ = svc.CompactSideChat(t.Context(), "session", "side")
			if corrupt {
				if _, err := recordWriter(t, svc).ExecContext(t.Context(), "UPDATE chat_side_contents SET content_json = '{}' "); err != nil {
					t.Fatal(err)
				}
			}
			restarted := New(Options{Store: db, DataDir: svc.dataDir, AppRunID: "new-launch", Sessions: sideSessionReader{}})
			if err := restarted.InitializeSideChats(t.Context()); err != nil {
				t.Fatal(err)
			}
			list, err := restarted.ListProtectedSides(t.Context())
			if err != nil || len(list) != 1 || list[0].SideID != "side" {
				t.Fatal(list, err)
			}
			if err := restarted.CloseProtectedSide(t.Context(), "side"); err != nil {
				t.Fatal(err)
			}
			list, err = restarted.ListProtectedSides(t.Context())
			if err != nil || len(list) != 0 {
				t.Fatal(list, err)
			}
			rows, _ := db.SideCompactions(t.Context(), false)
			if len(rows) != 1 || !rows[0].Closed || len(rows[0].Content) != 0 || rows[0].State != domain.SideCompactionUnknown {
				t.Fatal(rows)
			}
			if err := restarted.PreflightShutdown(t.Context()); err != nil {
				t.Fatal("closed side still vetoes", err)
			}
			restarted.sides.cancel()
		})
	}
}

func TestProtectedSideReadOnlyConversation(t *testing.T) {
	c := &protectedSideConversation{ChatConversation: &isolationConversation{recordCompactionConversation: &recordCompactionConversation{}}}
	ops := []func() error{
		func() error { _, err := c.SendTurn(t.Context(), ports.ChatUserMessage{}); return err },
		func() error { return c.Interrupt(t.Context(), "") },
		func() error { return c.ResolveRequest(t.Context(), "", ports.ChatDecision{}) },
		func() error { _, err := c.Compact(t.Context()); return err },
		func() error { return c.Rollback(t.Context(), "") },
		func() error { _, err := c.Fork(t.Context(), nil); return err },
		func() error { return c.SetTitle(t.Context(), "") },
		func() error { _, err := c.Steer(t.Context(), "", ports.ChatUserMessage{}); return err },
	}
	for i, op := range ops {
		if err := op(); !errors.Is(err, ErrNativeConversationProtected) {
			t.Fatal(i, err)
		}
	}
}

func TestProtectedSideDurableContentsSurviveNewLaunch(t *testing.T) {
	svc, db, _, _ := newRecordHarness(t, domain.HarnessClaudeCode)
	_, _ = svc.CompactSideChat(t.Context(), "session", "side")
	rows, _ := db.SideCompactions(t.Context(), true)
	var content durableSideContent
	if err := json.Unmarshal(rows[0].Content, &content); err != nil {
		t.Fatal(err)
	}
	restarted := New(Options{Store: db, DataDir: svc.dataDir, AppRunID: "new-launch", Sessions: sideSessionReader{}})
	if err := restarted.InitializeSideChats(t.Context()); err != nil {
		t.Fatal(err)
	}
	store := restarted.sides.store.(*memorySideStore)
	store.mu.Lock()
	draft := store.drafts["side"]
	store.mu.Unlock()
	if draft != content.Record.Draft || draft != "draft before" {
		t.Fatal(draft)
	}
	if len(restarted.sides.runtimes) != 0 {
		t.Fatal("new launch started protected host")
	}
	restarted.sides.cancel()
}

type protectedParentReader struct {
	mode              domain.SessionMode
	found, terminated bool
}

func (r protectedParentReader) GetSession(_ context.Context, id domain.SessionID) (domain.SessionRecord, bool, error) {
	return domain.SessionRecord{ID: id, Mode: r.mode, IsTerminated: r.terminated}, r.found, nil
}
func TestProtectedSideOrphanFlags(t *testing.T) {
	for _, tc := range []struct {
		name   string
		parent protectedParentReader
		orphan bool
	}{
		{"missing", protectedParentReader{}, true},
		{"terminated", protectedParentReader{mode: domain.SessionModeChat, found: true, terminated: true}, true},
		{"tui", protectedParentReader{mode: domain.SessionModeTUI, found: true}, true},
		{"chat-parent-old-launch", protectedParentReader{mode: domain.SessionModeChat, found: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, _, _, _ := newRecordHarness(t, domain.HarnessCodex)
			_, _ = svc.CompactSideChat(t.Context(), "session", "side")
			svc.sessions = tc.parent
			list, err := svc.ListProtectedSides(t.Context())
			if err != nil || len(list) != 1 || list[0].Orphan != tc.orphan {
				t.Fatal(list, err)
			}
			if err := svc.CloseProtectedSide(t.Context(), "side"); err != nil {
				t.Fatal("global close used parent ownership", err)
			}
		})
	}
}
func TestProtectedNativeDeleteForkBoundary(t *testing.T) {
	svc, _, conv, _ := newRecordHarness(t, domain.HarnessCodex)
	_, _ = svc.CompactSideChat(t.Context(), "session", "side")
	spy := &isolationConversation{recordCompactionConversation: conv}
	svc.controllers["session"] = &Controller{conv: spy, harness: domain.HarnessCodex}
	store := svc.sides.store.(*memorySideStore)
	side, _ := store.SideConversation(t.Context(), "side")
	// Clear side ownership metadata to exercise the conversation-layer key guard.
	side.ID = "cleanup-owner"
	svc.sides.deleteRegisteredFork(t.Context(), side, "provider-side-1")
	if spy.deletes != 0 {
		t.Fatal("deleted protected native on main conversation")
	}
	svc.sides.deleteRegisteredFork(t.Context(), side, "other-thread")
	if spy.deletes != 1 {
		t.Fatal("unrelated native thread blocked", spy.deletes)
	}
}

type protectedReconnectDriver struct {
	*nativeCountingDriver
	conv        ports.ChatConversation
	reconnected int
}

func (d *protectedReconnectDriver) Reconnect(_ context.Context, cfg ports.ChatResumeConfig) (ports.ChatConversation, error) {
	if !cfg.ReconnectOnly {
		return nil, errors.New("missing reconnect-only")
	}
	d.reconnected++
	return d.conv, nil
}

type protectedReplayConversation struct {
	*recordCompactionConversation
	events    chan ports.ChatEvent
	activated chan string
}

func (c *protectedReplayConversation) Events() <-chan ports.ChatEvent { return c.events }
func (c *protectedReplayConversation) ActivateLiveReconnect(_ context.Context, id string) error {
	c.activated <- id
	return nil
}
func TestProtectedSideReconnectObservesOriginalResult(t *testing.T) {
	svc, db, _, _ := newRecordHarness(t, domain.HarnessCodex)
	_, _ = svc.CompactSideChat(t.Context(), "session", "side")
	rows, _ := db.SideCompactions(t.Context(), true)
	row := rows[0]
	delete(svc.sides.runtimes, "side")
	conv := &protectedReplayConversation{recordCompactionConversation: &recordCompactionConversation{}, events: make(chan ports.ChatEvent, 2), activated: make(chan string, 1)}
	driver := &protectedReconnectDriver{nativeCountingDriver: &nativeCountingDriver{}, conv: conv}
	reg := registry.New(driver)
	reg.SetNativeGuard(svc.nativeGuard)
	svc.drivers = reg
	if err := svc.RetrySideConnection(t.Context(), "session", "side"); err != nil {
		t.Fatal(err)
	}
	if driver.reconnected != 1 || driver.calls.Load() != 0 {
		t.Fatal("retry started/resumed native", driver.reconnected, driver.calls.Load())
	}
	if id := <-conv.activated; id != row.ID {
		t.Fatal("wrong replay", id)
	}
	svc.sides.mu.Lock()
	runtime := svc.sides.runtimes["side"]
	svc.sides.mu.Unlock()
	if _, err := runtime.conv.SendTurn(t.Context(), ports.ChatUserMessage{}); !errors.Is(err, ErrNativeConversationProtected) {
		t.Fatal(err)
	}
	conv.events <- ports.ChatEvent{ProviderTurnID: "compact-result", CompactionID: row.ID, Compaction: &ports.ChatCompactionReceipt{CommandID: row.ID, Outcome: ports.ChatCompactionSettled}}
	deadline := time.Now().Add(3 * time.Second)
	for {
		remaining, err := db.SideCompactions(t.Context(), true)
		if err != nil {
			t.Fatal(err)
		}
		if len(remaining) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("original receipt did not settle", remaining)
		}
		time.Sleep(time.Millisecond)
	}
	runtime.mu.Lock()
	blocked := runtime.dispatchBlocked
	runtime.mu.Unlock()
	if !blocked {
		t.Fatal("query-only connection entered dispatch")
	}
	svc.sides.cancel()
}
