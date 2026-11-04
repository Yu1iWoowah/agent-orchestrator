package chat

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

type recordCompactionConversation struct {
	recoveringCompactionConversation
	io    func()
	count int
}

const imageReceipt = `{"clientMessageId":"draft-client","request":{"text":"pending","attachments":[{"mimeType":"image/png","data":"aW1hZ2UgYnl0ZXM="}]},"submitted":{"text":"pending","attachments":[],"references":[]}}`
const receiptDraft = `{"version":1,"text":"draft before","attachments":[{"id":"file","path":"image.png","name":"image.png","mimeType":"image/png","bytes":11}],"references":[],"pendingDelivery":` + imageReceipt + `,"pendingCommand":{"creationKey":"create","receipt":` + imageReceipt + `},"lastHandoffDelivery":` + imageReceipt + `}`

func (c *recordCompactionConversation) Compact(context.Context) (ports.ChatCompactionResult, error) {
	c.count++
	if c.io != nil {
		c.io()
	}
	return ports.ChatCompactionResult{}, c.compactErr
}

func newRecordHarness(t *testing.T, provider domain.AgentHarness) (*Service, *sqlite.Store, *recordCompactionConversation, string) {
	t.Helper()
	dir := t.TempDir()
	db := sqlitetest.MustOpenAt(t, dir)
	home := t.TempDir()
	link := filepath.Join(t.TempDir(), "provider-home")
	if err := os.Symlink(home, link); err != nil {
		t.Fatal(err)
	}
	svc, conv := seedRecordService(t, db, dir, provider, link)
	return svc, db, conv, home
}

func seedRecordService(t *testing.T, db *sqlite.Store, dir string, provider domain.AgentHarness, link string) (*Service, *recordCompactionConversation) {
	t.Helper()
	svc := New(Options{Store: db, DataDir: dir, AppRunID: "run", Sessions: sideSessionReader{}})
	store := svc.sides.store.(*memorySideStore)
	ctx := context.Background()
	_, _ = store.ClaimSideLaunch(ctx, "run", time.Now())
	env := map[string]string{"CODEX_HOME": link, "CLAUDE_CONFIG_DIR": link}
	raw, _ := json.Marshal(frozenSideConfig{Env: env, Permissions: domain.PermissionModeBypassPermissions})
	side := domain.SideConversation{ID: "side", SessionID: "session", MainConversationID: "main", AppRunID: "run", ProviderHostID: "btw-run-side", ProviderForkID: "provider-side-1", Generation: "g", Harness: provider, PolicyVersion: sidePolicyVersion, State: "ready", LaunchConfig: raw}
	if _, _, err := store.CreateSideConversation(ctx, side); err != nil {
		t.Fatal(err)
	}
	if err := store.SetSideDraft(ctx, side.ID, "run", "draft before", time.Now()); err != nil {
		t.Fatal(err)
	}
	conv := &recordCompactionConversation{}
	conv.compactErr = errors.New("provider reply unavailable")
	svc.sides.runtimes[side.ID] = &sideRuntime{side: side, conv: conv, ctx: ctx, cancel: func() {}}
	return svc, conv
}

func TestSideCompactionRegistrationPrecedesIO(t *testing.T) {
	ctx := context.Background()
	for _, provider := range []domain.AgentHarness{domain.HarnessCodex, domain.HarnessClaudeCode} {
		t.Run(string(provider), func(t *testing.T) {
			svc, db, conv, home := newRecordHarness(t, provider)
			conv.io = func() {
				rows, err := db.SideCompactions(ctx, true)
				if err != nil || len(rows) != 1 || rows[0].NativeHome != home || rows[0].NativeID != "provider-side-1" || rows[0].Provider != provider || rows[0].PermissionMode != domain.PermissionModeBypassPermissions || rows[0].State != domain.SideCompactionPending || !bytes.Contains(rows[0].Content, []byte("draft before")) {
					t.Fatalf("before I/O: %+v, %v", rows, err)
				}
			}
			if _, err := svc.CompactSideChat(ctx, "session", "side"); err == nil || conv.count != 1 {
				t.Fatalf("unknown result = %v, I/O = %d", err, conv.count)
			}
		})
	}
}

func TestSideCompactionCommitFailureHasNoIO(t *testing.T) {
	ctx := context.Background()
	svc, db, conv, _ := newRecordHarness(t, domain.HarnessCodex)
	dir := svc.dataDir
	writer, err := sql.Open("sqlite", filepath.Join(dir, "ao.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err := writer.Exec(`CREATE TRIGGER refuse_side_content BEFORE INSERT ON chat_side_contents BEGIN SELECT RAISE(ABORT,'test commit failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CompactSideChat(ctx, "session", "side"); err == nil || conv.count != 0 {
		t.Fatalf("commit failure = %v, I/O = %d", err, conv.count)
	}
	rows, err := db.SideCompactions(ctx, false)
	if err != nil || len(rows) != 0 {
		t.Fatalf("partially registered obligation: %+v, %v", rows, err)
	}
	var contents int
	if err := writer.QueryRow(`SELECT count(*) FROM chat_side_contents`).Scan(&contents); err != nil || contents != 0 {
		t.Fatalf("partially registered content = %d, %v", contents, err)
	}
}

func TestSideCompactionDurableUpdatesAndRestart(t *testing.T) {
	ctx := context.Background()
	svc, db, _, _ := newRecordHarness(t, domain.HarnessClaudeCode)
	_, _ = svc.CompactSideChat(ctx, "session", "side")
	store := svc.sides.store.(*memorySideStore)
	if err := store.SetSideDraft(ctx, "side", "run", "draft after", time.Now()); err != nil {
		t.Fatal(err)
	}
	turn := domain.SideTurn{ID: "question", SideID: "side", ClientMessageID: "client", Text: "new intake", Content: []domain.SideContent{{Type: "image", MIMEType: "image/png", Data: "aW1hZ2UgYnl0ZXM="}, {Type: "resource_link", URI: "file:///document", Name: "document"}}, CreatedAt: time.Now()}
	if _, _, err := store.ReserveSideTurn(ctx, turn, "run"); err != nil {
		t.Fatal(err)
	}
	rows, err := db.SideCompactions(ctx, true)
	if err != nil || len(rows) != 1 || bytes.Contains(rows[0].Content, []byte("aW1hZ2UgYnl0ZXM=")) {
		t.Fatalf("attachment body stored in database: %v, %v", rows, err)
	}
	// Rebuild from SQLite alone: no Electron recovery payload or live runtime.
	after := New(Options{Store: db, DataDir: svc.dataDir, AppRunID: "new-run", Sessions: sideSessionReader{}})
	projection := after.sides.store.(*memorySideStore)
	if err := projection.loadCompactions(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := projection.ClaimSideLaunch(ctx, "new-run", time.Now()); err != nil {
		t.Fatal(err)
	}
	draft, _ := projection.SideDraft(ctx, "side")
	saved, err := projection.SideTurn(ctx, "side", turn.ID)
	if err != nil || draft != "draft after" || saved.Text != turn.Text || saved.Content[0].Data != turn.Content[0].Data || saved.Content[1].URI != turn.Content[1].URI || len(after.sides.runtimes) != 0 || !projection.protected["side"] {
		t.Fatalf("restart lost content: draft=%q turn=%+v err=%v", draft, saved, err)
	}
}

type crashAfterRegistration struct{ ports.SideCompactionStore }

func (s crashAfterRegistration) RegisterSideCompaction(ctx context.Context, r domain.SideCompaction) error {
	if err := s.SideCompactionStore.RegisterSideCompaction(ctx, r); err != nil {
		return err
	}
	os.Exit(79)
	return nil
}

func TestSideCompactionCrashHelper(t *testing.T) {
	dir := os.Getenv("PB_TEST_CRASH_DIR")
	if dir == "" {
		return
	}
	db, err := sqlite.OpenPreMigrated(dir)
	if err != nil {
		t.Fatal(err)
	}
	svc, conv := seedRecordService(t, db, dir, domain.HarnessClaudeCode, dir)
	store := svc.sides.store.(*memorySideStore)
	ctx := context.Background()
	turn := domain.SideTurn{ID: "question", SideID: "side", ClientMessageID: "client", Text: "pending inbox", Content: []domain.SideContent{{Type: "image", MIMEType: "image/png", Data: "aW1hZ2UgYnl0ZXM="}, {Type: "resource_link", URI: "file:///document", Name: "document"}}, CreatedAt: time.Now()}
	if _, _, err := store.ReserveSideTurn(ctx, turn, "run"); err != nil {
		t.Fatal(err)
	}
	point := os.Getenv("PB_TEST_CRASH_POINT")
	if err := store.SetSideDraft(ctx, "side", "run", receiptDraft, time.Now()); err != nil {
		t.Fatal(err)
	}
	if point == "before_commit" {
		writer, err := sql.Open("sqlite", filepath.Join(dir, "ao.db"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Exec(`CREATE TRIGGER refuse_side_content BEFORE INSERT ON chat_side_contents BEGIN SELECT RAISE(ABORT,'test commit failure'); END`); err != nil {
			t.Fatal(err)
		}
	}
	if point == "after_commit" {
		store.durable = crashAfterRegistration{store.durable}
	}
	conv.io = func() {
		if err := os.WriteFile(filepath.Join(dir, "provider-io"), []byte("1"), 0o600); err != nil {
			t.Fatal(err)
		}
		if point == "after_io" {
			os.Exit(79)
		}
	}
	_, err = svc.CompactSideChat(ctx, "session", "side")
	if err == nil {
		t.Fatal("expected interrupted compaction")
	}
	os.Exit(79)
}

func TestSideCompactionCrashWindows(t *testing.T) {
	for _, point := range []string{"before_commit", "after_commit", "after_io", "after_unknown"} {
		t.Run(point, func(t *testing.T) {
			dir := t.TempDir()
			db, err := sqlitetest.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestSideCompactionCrashHelper$")
			cmd.Env = append(os.Environ(), "PB_TEST_CRASH_DIR="+dir, "PB_TEST_CRASH_POINT="+point)
			output, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 79 {
				t.Fatalf("crash helper: %v\n%s", err, output)
			}
			db, err = sqlite.OpenPreMigrated(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			ctx := context.Background()
			rows, err := db.SideCompactions(ctx, false)
			if err != nil {
				t.Fatal(err)
			}
			if point == "before_commit" {
				if len(rows) != 0 {
					t.Fatalf("partial registration: %+v", rows)
				}
				writer, err := sql.Open("sqlite", filepath.Join(dir, "ao.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer writer.Close()
				var count int
				if err := writer.QueryRow(`SELECT count(*) FROM chat_side_contents`).Scan(&count); err != nil || count != 0 {
					t.Fatalf("partial content: %d, %v", count, err)
				}
			} else {
				state := domain.SideCompactionPending
				if point == "after_unknown" {
					state = domain.SideCompactionUnknown
				}
				if len(rows) != 1 || rows[0].State != state || rows[0].Historical {
					t.Fatalf("crash result = %+v", rows)
				}
			}
			svc := New(Options{Store: db, DataDir: dir, AppRunID: "fresh", Sessions: sideSessionReader{}})
			defer svc.sides.stopAll()
			if err := svc.ClaimSideChatLaunch(ctx, "fresh"); err != nil {
				t.Fatal(err)
			}
			store := svc.sides.store.(*memorySideStore)
			if len(svc.sides.runtimes) != 0 {
				t.Fatal("restart created a provider runtime")
			}
			if point != "before_commit" {
				draft, _ := store.SideDraft(ctx, "side")
				turn, err := store.SideTurn(ctx, "side", "question")
				if err != nil || draft != receiptDraft || turn.Text != "pending inbox" || turn.Content[0].Data != "aW1hZ2UgYnl0ZXM=" || turn.Content[1].URI != "file:///document" {
					t.Fatalf("lost content: draft=%q turn=%+v err=%v", draft, turn, err)
				}
			}
			_, ioErr := os.Stat(filepath.Join(dir, "provider-io"))
			if (point == "before_commit" || point == "after_commit") != errors.Is(ioErr, os.ErrNotExist) {
				t.Fatalf("provider I/O at %s: %v", point, ioErr)
			}
		})
	}
}

func TestSideCompactionDraftReceiptImagesUseReferences(t *testing.T) {
	ctx := context.Background()
	svc, db, _, _ := newRecordHarness(t, domain.HarnessClaudeCode)
	if err := svc.SaveSideDraft(ctx, "session", "side", receiptDraft); err != nil {
		t.Fatal(err)
	}
	_, _ = svc.CompactSideChat(ctx, "session", "side")
	updated := strings.Replace(receiptDraft, `"text":"draft before"`, `"text":"draft after"`, 1)
	if err := svc.SaveSideDraft(ctx, "session", "side", updated); err != nil {
		t.Fatal(err)
	}
	rows, err := db.SideCompactions(ctx, true)
	if err != nil || len(rows) != 1 || bytes.Contains(rows[0].Content, []byte("aW1hZ2UgYnl0ZXM=")) {
		t.Fatalf("draft request image body stored in DB: %+v, %v", rows, err)
	}
	after := New(Options{Store: db, DataDir: svc.dataDir, AppRunID: "new-run", Sessions: sideSessionReader{}})
	projection := after.sides.store.(*memorySideStore)
	if err := projection.loadCompactions(ctx); err != nil {
		t.Fatal(err)
	}
	draft, _ := projection.SideDraft(ctx, "side")
	if draft != updated {
		t.Fatalf("draft receipt did not round trip: %s", draft)
	}
}

func recordWriter(t *testing.T, svc *Service) *sql.DB {
	t.Helper()
	writer, err := sql.Open("sqlite", filepath.Join(svc.dataDir, "ao.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	return writer
}

func TestSideCompactionRollbackPreservesEarlierContent(t *testing.T) {
	svc, db, conv, _ := newRecordHarness(t, domain.HarnessCodex)
	ctx := context.Background()
	previous := domain.SideCompaction{ID: "earlier", SideID: "side", NativeHome: svc.dataDir, NativeID: "earlier-native", State: domain.SideCompactionPending, Content: json.RawMessage(`{"draft":"previous commit"}`)}
	if err := db.RegisterSideCompaction(ctx, previous); err != nil {
		t.Fatal(err)
	}
	if _, err := recordWriter(t, svc).Exec(`CREATE TRIGGER refuse_side_content BEFORE INSERT ON chat_side_contents BEGIN SELECT RAISE(ABORT,'test commit failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CompactSideChat(ctx, "session", "side"); err == nil || conv.count != 0 {
		t.Fatalf("failed registration = %v, I/O = %d", err, conv.count)
	}
	rows, err := db.SideCompactions(ctx, false)
	if err != nil || len(rows) != 1 || rows[0].ID != previous.ID || !bytes.Equal(rows[0].Content, previous.Content) {
		t.Fatalf("earlier content changed: %+v, %v", rows, err)
	}
}

func TestSideCompactionFailedContentUpdateIsNotAccepted(t *testing.T) {
	svc, db, _, _ := newRecordHarness(t, domain.HarnessCodex)
	ctx := context.Background()
	_, _ = svc.CompactSideChat(ctx, "session", "side")
	writer := recordWriter(t, svc)
	if _, err := writer.Exec(`CREATE TRIGGER refuse_side_update BEFORE UPDATE ON chat_side_contents BEGIN SELECT RAISE(ABORT,'test update failure'); END`); err != nil {
		t.Fatal(err)
	}
	store := svc.sides.store.(*memorySideStore)
	if err := svc.SaveSideDraft(ctx, "session", "side", "uncommitted draft"); err == nil {
		t.Fatal("accepted memory-only draft")
	}
	turn := domain.SideTurn{ID: "uncommitted", SideID: "side", ClientMessageID: "client", Text: "uncommitted intake"}
	if _, _, err := store.ReserveSideTurn(ctx, turn, "run"); err == nil {
		t.Fatal("accepted memory-only intake")
	}
	draft, _ := store.SideDraft(ctx, "side")
	if _, err := store.SideTurn(ctx, "side", turn.ID); err == nil || draft != "draft before" {
		t.Fatalf("failed edit remained visible: draft=%q err=%v", draft, err)
	}
	rows, err := db.SideCompactions(ctx, true)
	if err != nil || len(rows) != 1 || bytes.Contains(rows[0].Content, []byte("uncommitted")) {
		t.Fatalf("failed edit changed durable content: %+v, %v", rows, err)
	}
}

func TestSideCompactionNormalTransitions(t *testing.T) {
	for _, state := range []domain.SideCompactionState{domain.SideCompactionSettled, domain.SideCompactionNotSent} {
		t.Run(string(state), func(t *testing.T) {
			svc, db, conv, _ := newRecordHarness(t, domain.HarnessCodex)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if state == domain.SideCompactionNotSent {
				conv.compactErr = ports.ErrChatCompactionNotSent
			}
			_, _ = svc.CompactSideChat(ctx, "session", "side")
			if state == domain.SideCompactionSettled {
				conv.events = make(chan ports.ChatEvent, 1)
				runtime := svc.sides.runtimes["side"]
				done := make(chan struct{})
				go func() { defer close(done); svc.sides.consumeEvents(ctx, runtime) }()
				defer func() { cancel(); <-done }()
				conv.events <- ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "compact-turn", CompactionID: runtime.compactionID, Compaction: &ports.ChatCompactionReceipt{CommandID: runtime.compactionID, Outcome: ports.ChatCompactionSettled}}
			}
			deadline := time.Now().Add(3 * time.Second)
			for {
				rows, err := db.SideCompactions(ctx, false)
				if err != nil {
					t.Fatal(err)
				}
				if len(rows) == 1 && rows[0].State == state {
					if len(rows[0].Content) != 0 {
						t.Fatal("terminal result retained side snapshot")
					}
					runtime := svc.sides.runtimes["side"]
					runtime.mu.Lock()
					released := !runtime.outboundUnverified && runtime.compactionID == ""
					runtime.mu.Unlock()
					if released {
						break
					}
				}
				if time.Now().After(deadline) {
					t.Fatalf("normal result not recorded: %+v", rows)
				}
				time.Sleep(time.Millisecond)
			}
			runtime := svc.sides.runtimes["side"]
			runtime.mu.Lock()
			blocked := runtime.outboundUnverified || runtime.compactionID != ""
			runtime.mu.Unlock()
			if blocked {
				t.Fatal("normal result did not release side")
			}
			if err := svc.ExplicitlyCloseSideCompaction(ctx, "session", "side"); err != nil {
				t.Fatal(err)
			}
			rows, err := db.SideCompactions(ctx, false)
			if err != nil || len(rows) != 1 || !rows[0].Closed || rows[0].State != state {
				t.Fatalf("closing a completed side lost metadata: %+v, %v", rows, err)
			}
		})
	}
}

func TestSideCompactionOnlyUserClosePersistsClosed(t *testing.T) {
	ctx := context.Background()
	svc, db, _, _ := newRecordHarness(t, domain.HarnessCodex)
	_, _ = svc.CompactSideChat(ctx, "session", "side")
	for _, close := range []func() error{
		func() error { return svc.RetireSideChatLaunch(ctx, "run") },
		func() error { return svc.Stop(ctx, "session") },
		func() error { return svc.StopForOwner(ctx, domain.SessionConversationOwner("session")) },
		func() error { return svc.CloseIndependentSideChat(ctx, "session", "side") },
	} {
		_ = close()
		rows, err := db.SideCompactions(ctx, true)
		if err != nil || len(rows) != 1 || rows[0].Closed || len(rows[0].Content) == 0 {
			t.Fatalf("non-user source closed obligation: %+v, %v", rows, err)
		}
	}
	stale, err := svc.ExportSideChatLaunch(ctx, "run")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.ExplicitlyCloseSideCompaction(ctx, "session", "side"); err != nil {
		t.Fatal(err)
	}
	if err := svc.CloseIndependentSideChat(ctx, "session", "side"); err != nil {
		t.Fatal(err)
	}
	rows, err := db.SideCompactions(ctx, false)
	if err != nil || len(rows) != 1 || !rows[0].Closed || rows[0].State != domain.SideCompactionUnknown || len(rows[0].Content) != 0 {
		t.Fatalf("explicit close metadata: %+v, %v", rows, err)
	}
	after := New(Options{Store: db, DataDir: svc.dataDir, AppRunID: "run", Sessions: sideSessionReader{}})
	projection := after.sides.store.(*memorySideStore)
	if err := projection.loadCompactions(ctx); err != nil {
		t.Fatal(err)
	}
	_, _ = projection.ClaimSideLaunch(ctx, "run", time.Now())
	if restored, err := projection.recover("run", stale, time.Now()); err != nil || len(restored) != 0 {
		t.Fatalf("closed side restored from stale Electron payload: %+v, %v", restored, err)
	}
}
