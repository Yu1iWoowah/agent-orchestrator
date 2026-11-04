package chat

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type reviewScopeConversation struct {
	*recordCompactionConversation
	scope    ports.ChatCompactionScope
	scopeErr error
	entered  chan struct{}
	release  chan struct{}
	nativeID string
}

func (c *reviewScopeConversation) CompactionScope(ctx context.Context) (ports.ChatCompactionScope, error) {
	if c.entered != nil {
		close(c.entered)
		select {
		case <-c.release:
		case <-ctx.Done():
			return ports.ChatCompactionScope{}, ctx.Err()
		}
	}
	return c.scope, c.scopeErr
}
func (c *reviewScopeConversation) ProviderConversationID() string {
	if c.nativeID != "" {
		return c.nativeID
	}
	return c.recordCompactionConversation.ProviderConversationID()
}

func addReviewSide(t *testing.T, svc *Service) domain.SideConversation {
	t.Helper()
	store := svc.sides.store.(*memorySideStore)
	side, err := store.SideConversation(t.Context(), "side")
	if err != nil {
		t.Fatal(err)
	}
	side.ID, side.SessionID, side.MainConversationID = "other", "other-session", "other-main"
	side.ProviderHostID, side.ProviderForkID = "btw-run-other", "other-native"
	if _, created, err := store.CreateSideConversation(t.Context(), side); err != nil || !created {
		t.Fatalf("create other side: %v, %v", created, err)
	}
	if err := store.SetSideDraft(t.Context(), side.ID, "run", "other draft", time.Now()); err != nil {
		t.Fatal(err)
	}
	return side
}

func TestSideCompactionScopeDoesNotBlockOtherSides(t *testing.T) {
	svc, _, conv, _ := newRecordHarness(t, domain.HarnessCodex)
	other := addReviewSide(t, svc)
	scope := &reviewScopeConversation{recordCompactionConversation: conv, entered: make(chan struct{}), release: make(chan struct{})}
	svc.sides.runtimes["side"].conv = scope
	compactDone := make(chan struct{})
	go func() { defer close(compactDone); _, _ = svc.CompactSideChat(t.Context(), "session", "side") }()
	defer func() { close(scope.release); <-compactDone }()
	select {
	case <-scope.entered:
	case <-time.After(time.Second):
		t.Fatal("scope query not started")
	}
	done := make(chan error, 1)
	go func() {
		draft, err := svc.SideDraft(t.Context(), other.SessionID, other.ID)
		if err == nil && draft != "other draft" {
			err = errors.New("other draft lost")
		}
		if err == nil {
			var sides []domain.SideConversation
			sides, err = svc.ListIndependentSideChats(t.Context(), other.SessionID)
			if err == nil && len(sides) != 1 {
				err = errors.New("other side not listed")
			}
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("scope query blocked another side's draft/list")
	}
}

func TestSideCompactionSameRunRestartNeverRestoresRuntime(t *testing.T) {
	svc, db, _, _ := newRecordHarness(t, domain.HarnessCodex)
	_, _ = svc.CompactSideChat(t.Context(), "session", "side")
	after := New(Options{Store: db, DataDir: svc.dataDir, AppRunID: "run", Sessions: sideSessionReader{}})
	defer after.sides.stopAll()
	if err := after.ClaimSideChatLaunch(t.Context(), "run"); err != nil {
		t.Fatal(err)
	}
	if _, err := after.SideSnapshot(t.Context(), "session", "side", time.Time{}, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := after.SendSideQuestion(t.Context(), "session", "side", ports.ChatUserMessage{Text: "queued after restart", ClientMessageID: "restart"}); !errors.Is(err, ErrSideNotReady) {
		t.Fatalf("send = %v, want not ready", err)
	}
	if err := after.RetrySideConnection(t.Context(), "session", "side"); err != nil {
		t.Fatal(err)
	}
	after.sides.mu.Lock()
	defer after.sides.mu.Unlock()
	if len(after.sides.runtimes) != 0 || len(after.sides.restoring) != 0 {
		t.Fatal("protected side created or scheduled a runtime")
	}
}

type reviewLogWriter struct{ lines chan string }

func (w reviewLogWriter) Write(p []byte) (int, error) {
	select {
	case w.lines <- string(p):
	default:
	}
	return len(p), nil
}
func reviewLogger() (*slog.Logger, <-chan string) {
	lines := make(chan string, 32)
	return slog.New(slog.NewTextHandler(reviewLogWriter{lines}, nil)), lines
}
func requireReviewWarning(t *testing.T, lines <-chan string, code string) {
	t.Helper()
	timeout := time.After(time.Second)
	for {
		select {
		case line := <-lines:
			if strings.Contains(line, code) {
				return
			}
		case <-timeout:
			t.Fatalf("missing warning %s", code)
		}
	}
}

func TestSideCompactionMissingImageIsolatesOnlyOneSide(t *testing.T) {
	svc, db, _, _ := newRecordHarness(t, domain.HarnessCodex)
	store := svc.sides.store.(*memorySideStore)
	if err := store.SetSideDraft(t.Context(), "side", "run", receiptDraft, time.Now()); err != nil {
		t.Fatal(err)
	}
	_, _ = svc.CompactSideChat(t.Context(), "session", "side")
	other := addReviewSide(t, svc)
	conv := &reviewScopeConversation{recordCompactionConversation: &recordCompactionConversation{}, nativeID: other.ProviderForkID}
	conv.compactErr = errors.New("unknown")
	svc.sides.runtimes[other.ID] = &sideRuntime{side: other, conv: conv, ctx: t.Context(), cancel: func() {}}
	_, _ = svc.CompactSideChat(t.Context(), other.SessionID, other.ID)
	if err := store.attachments.RemoveSession(t.Context(), "btw-run-side"); err != nil {
		t.Fatal(err)
	}
	log, lines := reviewLogger()
	after := New(Options{Store: db, DataDir: svc.dataDir, AppRunID: "run", Sessions: sideSessionReader{}, Log: log})
	defer after.sides.stopAll()
	if err := after.ClaimSideChatLaunch(t.Context(), "run"); err != nil {
		t.Fatalf("one missing image disabled all claims: %v", err)
	}
	requireReviewWarning(t, lines, "CHAT_SIDE_COMPACTION_RESTORE_FAILED")
	if draft, err := after.SideDraft(t.Context(), other.SessionID, other.ID); err != nil || draft != "other draft" {
		t.Fatalf("healthy side draft=%q, %v", draft, err)
	}
	if err := after.SaveSideDraft(t.Context(), other.SessionID, other.ID, "healthy edit"); err != nil {
		t.Fatal(err)
	}
	projection := after.sides.store.(*memorySideStore)
	projection.mu.Lock()
	protected := projection.protected["side"]
	projection.mu.Unlock()
	if !protected {
		t.Fatal("damaged side lost its fail-closed protection")
	}
	if ids := projection.unverifiedSides(); !containsReviewID(ids, "side") {
		t.Fatalf("damaged side missing from global protection: %v", ids)
	}
	rows, err := db.SideCompactions(t.Context(), true)
	if err != nil || len(rows) != 2 {
		t.Fatalf("obligations lost: %v, %v", rows, err)
	}
}
func containsReviewID(ids []string, id string) bool {
	for _, candidate := range ids {
		if candidate == id {
			return true
		}
	}
	return false
}

func TestSideCompactionAttachmentCleanupIsBestEffort(t *testing.T) {
	svc, _, _, _ := newRecordHarness(t, domain.HarnessCodex)
	_, _ = svc.CompactSideChat(t.Context(), "session", "side")
	if err := svc.ExplicitlyCloseSideCompaction(t.Context(), "session", "side"); err != nil {
		t.Fatal(err)
	}
	log, lines := reviewLogger()
	svc.log = log
	svc.stopProviderHost = func(context.Context, domain.SessionID) error { return nil }
	deleter := &sideCleanupTestConversation{}
	svc.controllers["session"] = &Controller{conv: deleter, harness: domain.HarnessCodex}
	_, notices, unwatch, err := svc.WatchSideChat(t.Context(), "session", "side")
	if err != nil {
		t.Fatal(err)
	}
	defer unwatch()
	root := filepath.Join(svc.dataDir, "attachments")
	if err := os.WriteFile(root, []byte("blocks canonical root"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := svc.CloseIndependentSideChat(t.Context(), "session", "side"); err != nil {
		t.Fatalf("cleanup failure interrupted close: %v", err)
	}
	requireReviewWarning(t, lines, "CHAT_SIDE_ATTACHMENTS_CLEANUP_FAILED")
	if len(deleter.deleted) != 1 || deleter.deleted[0] != "provider-side-1" {
		t.Fatalf("fork not deleted: %v", deleter.deleted)
	}
	select {
	case <-notices:
	default:
		t.Fatal("close was not announced")
	}
	select {
	case <-svc.sides.wake:
	default:
		t.Fatal("close did not signal scheduler")
	}
}

func TestSideCompactionRetirementRemovesImages(t *testing.T) {
	svc, db, _, _ := newRecordHarness(t, domain.HarnessCodex)
	store := svc.sides.store.(*memorySideStore)
	if err := store.SetSideDraft(t.Context(), "side", "run", receiptDraft, time.Now()); err != nil {
		t.Fatal(err)
	}
	_, _ = svc.CompactSideChat(t.Context(), "session", "side")
	rows, err := db.SideCompactions(t.Context(), true)
	if err != nil || len(rows) != 1 {
		t.Fatalf("record: %v, %v", rows, err)
	}
	var saved durableSideContent
	if err := json.Unmarshal(rows[0].Content, &saved); err != nil {
		t.Fatal(err)
	}
	var image string
	for _, name := range saved.DraftImages {
		image = name
	}
	if image == "" {
		t.Fatal("image fixture was not persisted")
	}
	if _, err := store.readImage("btw-run-side", image); err != nil {
		t.Fatal(err)
	}
	runtime := svc.sides.runtimes["side"]
	if err := svc.sides.transitionCompaction(t.Context(), runtime, domain.SideCompactionSettled); err != nil {
		t.Fatal(err)
	}
	runtime.mu.Lock()
	runtime.compactionID, runtime.compactionEpoch = "", ""
	svc.sides.setOutboundUnverifiedLocked(runtime, false)
	runtime.mu.Unlock()
	defer svc.sides.stopAll()
	if err := svc.sides.claim(t.Context(), "new-run"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.readImage("btw-run-side", image); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retired image remains: %v", err)
	}
}

func TestSideCompactionRejectedWritePreservesProjection(t *testing.T) {
	for _, boundary := range []string{"pending", "uncertain"} {
		t.Run(boundary, func(t *testing.T) {
			svc, _, _, _ := newRecordHarness(t, domain.HarnessCodex)
			_, _ = svc.CompactSideChat(t.Context(), "session", "side")
			store := svc.sides.store.(*memorySideStore)
			store.mu.Lock()
			side := store.sides["side"]
			side.BoundaryState, side.RecreateRequired = boundary, false
			store.sides["side"] = side
			store.mu.Unlock()
			if err := store.UpsertSideMessage(t.Context(), domain.SideMessage{ID: "late", SideID: "side"}, "wrong-generation"); !errors.Is(err, ErrSideClosed) {
				t.Fatalf("rejected edit = %v", err)
			}
			if _, err := recordWriter(t, svc).Exec(`CREATE TRIGGER refuse_side_update BEFORE UPDATE ON chat_side_contents BEGIN SELECT RAISE(ABORT,'test update failure'); END`); err != nil {
				t.Fatal(err)
			}
			if err := store.SetSideDraft(t.Context(), "side", "run", "uncommitted", time.Now()); err == nil {
				t.Fatal("durable failure accepted")
			}
			restored, err := store.SideConversation(t.Context(), "side")
			if err != nil || !reflect.DeepEqual(restored, side) {
				t.Fatalf("failed edit changed projection: before=%+v after=%+v err=%v", side, restored, err)
			}
		})
	}
}

type reviewTransitionFailure struct{ ports.SideCompactionStore }

func (reviewTransitionFailure) TransitionSideCompaction(context.Context, string, string, domain.SideCompactionState, bool, bool) error {
	return errors.New("durable result unavailable")
}
func TestSideCompactionResultFailureKeepsReceipt(t *testing.T) {
	svc, db, conv, _ := newRecordHarness(t, domain.HarnessCodex)
	_, _ = svc.CompactSideChat(t.Context(), "session", "side")
	log, lines := reviewLogger()
	svc.log = log
	store := svc.sides.store.(*memorySideStore)
	store.durable = reviewTransitionFailure{store.durable}
	conv.events, conv.acknowledged = make(chan ports.ChatEvent, 1), make(chan string, 1)
	runtime := svc.sides.runtimes["side"]
	id := runtime.compactionID
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); svc.sides.consumeEvents(ctx, runtime) }()
	defer func() { cancel(); <-done }()
	conv.events <- ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "compact-turn", ProviderEventID: "receipt", CompactionID: id, Compaction: &ports.ChatCompactionReceipt{CommandID: id, Outcome: ports.ChatCompactionSettled}}
	requireReviewWarning(t, lines, "CHAT_SIDE_COMPACTION_RESULT_FAILED")
	cancel()
	<-done
	runtime.mu.Lock()
	protected := runtime.outboundUnverified && runtime.compactionID == id
	runtime.mu.Unlock()
	if !protected {
		t.Fatal("failed result persistence released the side")
	}
	select {
	case <-conv.acknowledged:
		t.Fatal("receipt acknowledged before durable settlement")
	default:
	}
	rows, err := db.SideCompactions(t.Context(), true)
	if err != nil || len(rows) != 1 || rows[0].State != domain.SideCompactionUnknown {
		t.Fatalf("failed transition changed durable result: %v, %v", rows, err)
	}
}

func TestSideCompactionScopeProvenance(t *testing.T) {
	for _, tc := range []struct {
		name, source, homeSource string
		scope                    ports.ChatCompactionScope
		err                      error
	}{
		{name: "unsupported", source: "not_reported", homeSource: "launch_config"},
		{name: "host", source: "host", homeSource: "host", scope: ports.ChatCompactionScope{Processes: &domain.CompactionProcessScope{HostPID: 123}}},
		{name: "missing identity", source: "host_missing_identity", homeSource: "host"},
		{name: "timeout", source: "host_timeout", homeSource: "launch_config", scope: ports.ChatCompactionScope{NativeHome: "discard", Processes: &domain.CompactionProcessScope{HostPID: 999}}, err: context.DeadlineExceeded},
		{name: "error", source: "host_error", homeSource: "launch_config", err: errors.New("host unreachable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, db, conv, home := newRecordHarness(t, domain.HarnessCodex)
			if tc.err == nil {
				tc.scope.NativeHome = home
			}
			scope := &reviewScopeConversation{recordCompactionConversation: conv, scope: tc.scope, scopeErr: tc.err}
			if tc.name != "unsupported" {
				svc.sides.runtimes["side"].conv = scope
			}
			log, lines := reviewLogger()
			svc.log = log
			_, _ = svc.CompactSideChat(t.Context(), "session", "side")
			rows, err := db.SideCompactions(t.Context(), true)
			if err != nil || len(rows) != 1 || rows[0].ScopeSource != tc.source || rows[0].NativeHomeSource != tc.homeSource || rows[0].NativeHome != home {
				t.Fatalf("scope provenance: %v, %v", rows, err)
			}
			if tc.err != nil {
				requireReviewWarning(t, lines, "CHAT_SIDE_COMPACTION_SCOPE_FAILED")
				if rows[0].Scope != nil {
					t.Fatal("failed host response retained partial identity")
				}
			}
		})
	}
}
