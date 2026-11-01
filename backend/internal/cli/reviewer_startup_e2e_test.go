//go:build e2e

package cli_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/persistenthost"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/processalive"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

func TestE2E_DaemonBindsPortWithRecoverableChatReviewer(t *testing.T) {
	e := newEnv(t)
	// Resolve a copy of this test binary, never a developer's installed Codex.
	providerDir := t.TempDir()
	name := "codex"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	code, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(providerDir, name), code, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", providerDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	marker := filepath.Join(providerDir, "provider-pid")
	// Registered before daemon cleanup so an asserted 409 is checked first.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		if err := persistenthost.ShutdownQuick(ctx, e.dataDir, "review-boot-review"); err != nil {
			t.Errorf("fixture host cleanup: %v", err)
		}
		raw, err := os.ReadFile(marker)
		if err != nil {
			t.Errorf("fake provider did not publish its PID: %v", err)
			return
		}
		pid, err := strconv.Atoi(string(raw))
		if err != nil {
			t.Error(err)
			return
		}
		if processalive.Alive(pid) {
			t.Errorf("fixture native provider still alive after host cleanup: pid=%d", pid)
		}
	})
	st, err := sqlitetest.Open(e.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	now := time.Now().UTC()
	workspace := t.TempDir()
	if err := st.UpsertProject(ctx, domain.ProjectRecord{
		ID: "boot", Kind: domain.ProjectKindScratch, Path: workspace, RegisteredAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	worker, err := st.CreateSession(ctx, domain.SessionRecord{
		ProjectID: "boot", Kind: domain.KindWorker, Harness: domain.HarnessCodex,
		Mode: domain.SessionModeChat, HibernatedAt: &now,
		Activity:  domain.Activity{State: domain.ActivityIdle, LastActivityAt: now},
		Metadata:  domain.SessionMetadata{WorkspacePath: workspace, ProviderConversationID: "worker-thread"},
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertReview(ctx, domain.Review{
		ID: "boot-review", SessionID: worker.ID, ProjectID: worker.ProjectID,
		Harness: domain.ReviewerCodex, InterfaceMode: domain.ReviewerInterfaceChat,
		ProviderConversationID: "missing-review-thread", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	// Recovery can fail for this deliberately missing thread. It must never
	// prevent the listener from becoming ready. Unsettled custody may refuse a
	// normal shutdown, which must leave the daemon available rather than deadlock.
	e.startDaemonWithShutdownCheck(t, true)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("review recovery did not start the isolated fake provider")
}

// The copied test executable supplies only the recovery protocol. It cannot run
// an inference request or resolve a real provider from the host machine.
func runReviewerStartupProvider() bool {
	name := filepath.Base(os.Args[0])
	if name != "codex" && name != "codex.exe" {
		return false
	}
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Println("codex-cli 0.121.0")
		return true
	}
	if len(os.Args) > 1 && os.Args[1] == "login" {
		fmt.Println("Logged in using a synthetic test account")
		return true
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(os.Args[0]), "provider-pid"), []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(scanner.Bytes(), &request) != nil || len(request.ID) == 0 {
			continue
		}
		if request.Method == "initialize" {
			fmt.Fprintf(os.Stdout, `{"jsonrpc":"2.0","id":%s,"result":{"userAgent":"recovery-fixture"}}`+"\n", request.ID)
		} else {
			fmt.Fprintf(os.Stdout, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32600,"message":"synthetic missing thread"}}`+"\n", request.ID)
		}
	}
	return true
}
