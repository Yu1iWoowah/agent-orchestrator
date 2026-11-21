package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/persistenthost"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

func TestPersistentChatHostKeepSetUsesDurableOwnership(t *testing.T) {
	before := time.Now().UTC()
	records := []domain.SessionRecord{
		{ID: "live-chat", Mode: domain.SessionModeChat, Harness: domain.HarnessCodex},
		{ID: "terminated-chat", Mode: domain.SessionModeChat, Harness: domain.HarnessCodex, IsTerminated: true},
		{ID: "tui", Mode: domain.SessionModeTUI, Harness: domain.HarnessCodex},
		{ID: "other-provider", Mode: domain.SessionModeChat, Harness: domain.HarnessClaudeCode},
		{ID: "hibernated-chat", Mode: domain.SessionModeChat, Harness: domain.HarnessCodex, HibernatedAt: &before},
	}
	keep := persistentChatHostKeepSet(records, []domain.Review{{ID: "recoverable-review"}})
	if len(keep) != 3 {
		t.Fatalf("keep = %v, want both live Chat providers and the reviewer", keep)
	}
	if _, ok := keep["live-chat"]; !ok {
		t.Fatalf("keep = %v, missing live-chat", keep)
	}
	if _, ok := keep["other-provider"]; !ok {
		t.Fatalf("keep = %v, missing other-provider", keep)
	}
	if _, ok := keep["review-recoverable-review"]; !ok {
		t.Fatalf("keep = %v, missing recoverable reviewer host", keep)
	}
}

func TestSideHostsSurviveOnlyTheirDesktopLaunch(t *testing.T) {
	dir := t.TempDir()
	for _, id := range []string{"btw-current-one", "btw-current-two", "btw-previous-one", "main"} {
		if err := os.MkdirAll(filepath.Join(dir, "chat-hosts", id), 0700); err != nil {
			t.Fatal(err)
		}
	}
	keep := map[string]struct{}{}
	keepLaunchSideHosts(dir, "current", keep)
	if len(keep) != 2 {
		t.Fatalf("kept side hosts: %v", keep)
	}
	if _, ok := keep["btw-previous-one"]; ok {
		t.Fatal("kept a previous desktop launch")
	}
}

// The keep set comes from the recoverable query, so the host of an exited chat
// reviewer with no running pass is no longer kept.
func TestPersistentChatHostKeepSetSkipsExitedChatReviewers(t *testing.T) {
	ctx := context.Background()
	store := sqlitetest.MustOpen(t)
	now := time.Now().UTC()
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "p", Path: "/repo/p", RegisteredAt: now}); err != nil {
		t.Fatal(err)
	}
	for _, review := range []domain.Review{
		{ID: "exited-done", ReviewerActivityState: domain.ActivityExited},
		{ID: "active", ReviewerActivityState: domain.ActivityActive},
	} {
		rec, err := store.CreateSession(ctx, domain.SessionRecord{ProjectID: "p", Kind: domain.KindWorker, Harness: domain.HarnessCodex, CreatedAt: now, UpdatedAt: now})
		if err != nil {
			t.Fatal(err)
		}
		review.SessionID, review.ProjectID, review.Harness = rec.ID, "p", domain.ReviewerCodex
		review.InterfaceMode, review.ProviderConversationID, review.CreatedAt, review.UpdatedAt = domain.ReviewerInterfaceChat, "provider-"+review.ID, now, now
		if err := store.UpsertReview(ctx, review); err != nil {
			t.Fatal(err)
		}
	}
	reviews, err := store.ListRecoverableChatReviews(ctx)
	if err != nil {
		t.Fatal(err)
	}
	keep := persistentChatHostKeepSet(nil, reviews)
	if _, ok := keep["review-exited-done"]; ok {
		t.Fatalf("keep = %v, holds the exited reviewer's host", keep)
	}
	if _, ok := keep["review-active"]; !ok || len(keep) != 1 {
		t.Fatalf("keep = %v, want only the active reviewer's host", keep)
	}
}

type stopReceiptHostStore struct {
	persistentChatSessionStore
	receipts []domain.ReviewerStopReceipt
	err      error
}

func (s stopReceiptHostStore) ListReviewerStopReceipts(context.Context) ([]domain.ReviewerStopReceipt, error) {
	return s.receipts, s.err
}

func TestPersistentChatReconcilePreservesUnsettledReviewerHost(t *testing.T) {
	root := t.TempDir()
	hostID := "review-held"
	hostDir := filepath.Join(root, "chat-hosts", hostID)
	if err := os.MkdirAll(hostDir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(hostDir, "host.json")
	descriptor := persistenthost.Descriptor{Version: persistenthost.ProtocolVersion, SessionID: hostID, PID: 0, Address: "unused", Token: "synthetic-fixture"}
	raw, err := json.Marshal(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	store := stopReceiptHostStore{
		persistentChatSessionStore: sqlitetest.MustOpen(t),
		receipts: []domain.ReviewerStopReceipt{
			{ReviewerStopKey: domain.ReviewerStopKey{ReviewID: "held"}, InterfaceMode: domain.ReviewerInterfaceChat},
		},
	}
	if err := reconcilePersistentChatHosts(t.Context(), root, store); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != string(raw) {
		t.Fatalf("boot reconcile removed exact host evidence: %v", err)
	}
	store.receipts = nil
	store.err = errors.New("receipts unavailable")
	if err := reconcilePersistentChatHosts(t.Context(), root, store); !errors.Is(err, store.err) {
		t.Fatalf("went ahead with an unreadable receipt set: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	store.err = nil
	if err := reconcilePersistentChatHosts(t.Context(), root, store); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("control fixture did not exercise host cleanup: %v", err)
	}
}
