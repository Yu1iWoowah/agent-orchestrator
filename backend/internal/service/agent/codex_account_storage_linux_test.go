package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestExplicitStorageRetryRevalidatesWithoutOpeningProvider(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "accounts-parent")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o775); err != nil {
		t.Fatal(err)
	}
	var opens atomic.Int32
	manager := newCodexAccountManager(context.Background(), filepath.Join(parent, "accounts"), filepath.Join(parent, "pending"), filepath.Join(parent, "staging"), filepath.Join(root, "unused-device"), &fakeCodexAccountFactory{open: func(ports.CodexAccountContext) (ports.CodexAccountClient, error) {
		opens.Add(1)
		return nil, errors.New("provider must not open")
	}}, nil, nil)
	svc := &Service{codexAccounts: manager}
	if err := svc.WaitCodexAccountStoreReady(context.Background()); err == nil {
		t.Fatal("writable ancestor accepted")
	}
	retry, ok := any(svc).(interface{ RetryCodexAccountStore(context.Context) error })
	if !ok {
		t.Fatal("explicit storage-only retry is unavailable")
	}
	if err := retry.RetryCodexAccountStore(context.Background()); err == nil {
		t.Fatal("retry bypassed unsafe storage")
	}
	if err := os.Chmod(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := svc.WaitCodexAccountStoreReady(context.Background()); err == nil {
		t.Fatal("ordinary readiness silently cleared safety latch")
	}
	if err := retry.RetryCodexAccountStore(context.Background()); err != nil {
		t.Fatalf("explicit repaired retry: %v", err)
	}
	if err := svc.WaitCodexAccountStoreReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	active := filepath.Join(manager.pendingRoot, "active-fake-login")
	if err := ensurePrivateDirectory(active); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(active, "fake-state")
	if err := os.WriteFile(marker, []byte("fake-pending-state"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := retry.RetryCodexAccountStore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "fake-pending-state" {
		t.Fatalf("retry changed active login storage: %v", err)
	}

	if err := os.Chmod(parent, 0o775); err != nil {
		t.Fatal(err)
	}
	if err := retry.RetryCodexAccountStore(context.Background()); err == nil {
		t.Fatal("explicit retry reused stale successful validation")
	}
	if err := os.Chmod(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := retry.RetryCodexAccountStore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "fake-pending-state" {
		t.Fatalf("repaired retry changed active login storage: %v", err)
	}

	if opens.Load() != 0 {
		t.Fatal("storage-only retry opened provider")
	}
	if _, err := os.Stat(filepath.Join(root, "unused-device")); !os.IsNotExist(err) {
		t.Fatalf("storage retry touched device home: %v", err)
	}
}
