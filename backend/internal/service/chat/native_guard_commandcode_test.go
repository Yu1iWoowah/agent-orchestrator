package chat

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/registry"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/nativehome"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type r1CommandHomeStore struct {
	ports.SideCompactionStore
	rows []domain.SideCompaction
}

func (s *r1CommandHomeStore) SideCompactions(context.Context, bool) ([]domain.SideCompaction, error) {
	return s.rows, nil
}

func TestAuditR1CommandCodeForkUsesFinalHostHome(t *testing.T) {
	cfg := ports.ChatStartConfig{SessionID: "existing-command-host", DataDir: t.TempDir(), WorkspacePath: t.TempDir(), Env: map[string]string{"HOME": t.TempDir()}}
	effective, err := nativehome.InvocationEnvironment(domain.HarnessCommandCode, cfg.Env, cfg.DataDir, cfg.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	home, err := nativehome.ResolveForOperation(t.Context(), domain.HarnessCommandCode, effective, cfg.WorkspacePath, cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	store := &r1CommandHomeStore{rows: []domain.SideCompaction{{ID: "command-compaction", Epoch: "original-epoch", NativeHome: home, NativeID: "protected-id", SideID: "protected-side", ProviderHostID: string(cfg.SessionID), State: domain.SideCompactionUnknown}}}
	counter := &nativeCountingDriver{}
	reg := registry.New(&nativeProviderDriver{nativeCountingDriver: counter, provider: domain.HarnessCommandCode})
	reg.SetNativeGuard(NewNativeGuard(store))
	driver, _ := reg.Driver(domain.HarnessCommandCode)
	_, err = driver.Resume(t.Context(), ports.ChatResumeConfig{SessionID: cfg.SessionID, ProviderConversationID: "protected-id", DataDir: cfg.DataDir, WorkspacePath: cfg.WorkspacePath, Env: cfg.Env})
	if !errors.Is(err, ErrNativeConversationProtected) || counter.calls.Load() != 0 {
		t.Fatalf("Resume control not protected: err=%v calls=%d", err, counter.calls.Load())
	}
	_, err = driver.(ports.ChatDriverReconnector).Reconnect(t.Context(), ports.ChatResumeConfig{SessionID: cfg.SessionID, ProviderConversationID: "protected-id", DataDir: cfg.DataDir, WorkspacePath: cfg.WorkspacePath, Env: cfg.Env})
	if !errors.Is(err, ErrNativeConversationProtected) || counter.calls.Load() != 0 {
		t.Fatalf("Reconnect control not protected: err=%v calls=%d", err, counter.calls.Load())
	}
	_, err = driver.(ports.ChatIsolatedForker).ForkIntoHost(t.Context(), "protected-id", "", cfg)
	if !errors.Is(err, ErrNativeConversationProtected) || counter.calls.Load() != 0 {
		t.Fatalf("Fork used a different home than final launch: err=%v provider calls=%d", err, counter.calls.Load())
	}
}
