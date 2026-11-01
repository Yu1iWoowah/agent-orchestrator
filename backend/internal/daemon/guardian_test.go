package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	usagesvc "github.com/aoagents/agent-orchestrator/backend/internal/service/usage"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

func TestGuardianRootsRejectUnavailableAccounts(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	now := time.Now().UTC()
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, "sessions"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, a := range []domain.AgentAccount{
		{ID: "enabled", Provider: domain.AgentAccountProviderCodex, Home: home, CreatedAt: now},
		{ID: "disabled", Provider: domain.AgentAccountProviderCodex, Home: home, CreatedAt: now, DisabledAt: &now},
		{ID: "retired", Provider: domain.AgentAccountProviderCodex, Home: home, CreatedAt: now, RetiredAt: &now},
		{ID: "wrong-provider", Provider: domain.AgentAccountProviderClaude, Home: home, CreatedAt: now},
		{ID: "missing-history", Provider: domain.AgentAccountProviderCodex, Home: t.TempDir(), CreatedAt: now},
	} {
		if a.ID != "enabled" && a.ID != "missing-history" {
			a.Home = filepath.Join(home, a.ID)
			if err := os.MkdirAll(filepath.Join(a.Home, "sessions"), 0700); err != nil {
				t.Fatal(err)
			}
		}
		if err := store.CreateAgentAccount(context.Background(), a); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"disabled", "retired", "wrong-provider", "missing-history", "unknown"} {
		t.Run(id, func(t *testing.T) {
			roots, err := guardianRoots(context.Background(), store, usagesvc.SourceRoots{}, domain.GuardianOwner{AccountID: id})
			if err == nil || len(roots) != 0 || err.Error() != "guardian account unavailable" {
				t.Fatalf("unavailable account roots=%v err=%v", roots, err)
			}
		})
	}
	roots, err := guardianRoots(context.Background(), store, usagesvc.SourceRoots{}, domain.GuardianOwner{AccountID: "enabled"})
	if err != nil || !reflect.DeepEqual(roots, []string{filepath.Join(home, "sessions")}) {
		t.Fatalf("active roots=%v err=%v", roots, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := guardianRoots(ctx, store, usagesvc.SourceRoots{}, domain.GuardianOwner{AccountID: "enabled"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled root resolution=%v", err)
	}
}

func TestGuardianRootsCanonicalizeOnlyRegisteredHistoryLinks(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	home := t.TempDir()
	shared := t.TempDir()
	for _, name := range []string{"sessions", "archived_sessions"} {
		if err := os.Mkdir(filepath.Join(shared, name), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(shared, name), filepath.Join(home, name)); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
	}
	if err := store.CreateAgentAccount(context.Background(), domain.AgentAccount{ID: "linked", Provider: domain.AgentAccountProviderCodex, Home: home, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	device := usagesvc.SourceRoots{CodexSessions: filepath.Join(home, "sessions"), CodexArchived: filepath.Join(home, "archived_sessions")}
	want := []string{filepath.Join(shared, "sessions"), filepath.Join(shared, "archived_sessions")}
	for _, id := range []string{"linked", domain.DeviceAgentAccountID(domain.AgentAccountProviderCodex)} {
		roots, err := guardianRoots(context.Background(), store, device, domain.GuardianOwner{AccountID: id})
		if err != nil || !reflect.DeepEqual(roots, want) {
			t.Fatalf("%s roots=%v err=%v", id, roots, err)
		}
	}
}

func TestGuardianRootsRejectMissingOrNonDirectoryDeviceHistory(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	path := filepath.Join(t.TempDir(), "sessions")
	if err := os.WriteFile(path, []byte("synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
	roots, err := guardianRoots(context.Background(), store, usagesvc.SourceRoots{CodexSessions: path, CodexArchived: path + "-missing"}, domain.GuardianOwner{AccountID: domain.DeviceAgentAccountID(domain.AgentAccountProviderCodex)})
	if err == nil || len(roots) != 0 {
		t.Fatalf("invalid device history=%v err=%v", roots, err)
	}
}

func TestGuardianRootsRejectBrokenLinkAlongsideValidRoot(t *testing.T) {
	valid := t.TempDir()
	broken := filepath.Join(t.TempDir(), "archived_sessions")
	if err := os.Symlink(filepath.Join(t.TempDir(), "gone"), broken); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	roots, err := guardianHistoryRoots([]string{valid, broken})
	if err == nil || len(roots) != 0 || err.Error() != "guardian account unavailable" {
		t.Fatalf("broken registered link skipped: %v %v", roots, err)
	}
}
