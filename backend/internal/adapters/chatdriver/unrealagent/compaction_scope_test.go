//go:build darwin || linux

package unrealagent

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/persistenthost"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
)

func TestUnrealScopeProviderHelper(t *testing.T) {
	if os.Getenv("AO_TEST_UNREAL_SCOPE_HELPER") != "1" {
		return
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
}

func TestUnrealForkCleanupUsesHostScope(t *testing.T) {
	dataDir, _ := nativeForkFixture(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg := persistenthost.Config{SessionID: "unreal-scope", DataDir: dataDir, Workdir: t.TempDir(), Protocol: persistenthost.ProtocolUnreal, OwnershipFingerprint: "scope-test",
		Env:  append(os.Environ(), "AO_TEST_UNREAL_SCOPE_HELPER=1", "AO_CHAT_HARNESS="+string(domain.HarnessUnreal)),
		Argv: []string{executable, "-test.run=^TestUnrealScopeProviderHelper$"}}
	done := make(chan error, 1)
	go func() { done <- persistenthost.Run(context.Background(), cfg) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = persistenthost.Shutdown(ctx, dataDir, cfg.SessionID)
		select {
		case <-done:
		case <-ctx.Done():
			t.Error("scope host did not stop")
		}
	})
	attachCfg := cfg
	attachCfg.ReconnectOnly = true
	var transport *persistenthost.Transport
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		transport, err = persistenthost.ConnectOrStart(t.Context(), attachCfg)
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer transport.Stdin.Close()
	conv := &conversation{cfg: providerConfig{DataDir: dataDir, ProviderConversationID: "parent"}, transport: transport}
	reader, ok := any(conv).(ports.ChatCompactionScopeReader)
	if !ok {
		t.Fatal("Unreal conversation does not expose its actual host scope")
	}
	scope, err := reader.CompactionScope(t.Context())
	home, homeErr := filepath.EvalSymlinks(nativeSessionDirectory(dataDir))
	if err != nil || homeErr != nil || scope.NativeHome != home || scope.NativeHomeError != "" {
		t.Fatalf("host scope=%+v err=%v root=%v", scope, err, homeErr)
	}
	for _, protected := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "protected"}[protected], func(t *testing.T) {
			id, err := forkNativeSession(t.Context(), dataDir, "parent", "ao-request-1")
			if err != nil {
				t.Fatal(err)
			}
			store := &unrealScopeStore{}
			if protected {
				store.rows = []domain.SideCompaction{{NativeHome: scope.NativeHome, NativeID: id}}
			}
			guard := chatsvc.NewNativeGuard(store)
			calls := 0
			err = guard.Run(t.Context(), domain.HarnessUnreal, ports.ChatResumeConfig{ProviderConversationID: id, DataDir: conv.cfg.DataDir}, func() error { calls++; return conv.DeleteFork(t.Context(), id) })
			_, statErr := os.Stat(filepath.Join(home, id+".session.jsonl"))
			if protected {
				if !errors.Is(err, ports.ErrNativeConversationProtected) || calls != 0 || statErr != nil {
					t.Fatalf("protected cleanup: calls=%d error=%v stat=%v", calls, err, statErr)
				}
			} else if err != nil || calls != 1 || !os.IsNotExist(statErr) {
				t.Fatalf("ordinary cleanup: calls=%d error=%v stat=%v", calls, err, statErr)
			}
		})
	}
}

type unrealScopeStore struct {
	ports.SideCompactionStore
	rows []domain.SideCompaction
}

func (s *unrealScopeStore) SideCompactions(context.Context, bool) ([]domain.SideCompaction, error) {
	return s.rows, nil
}

func TestUnrealScopeWithoutHostRemainsUnavailable(t *testing.T) {
	for _, conv := range []*conversation{{}, {transport: &persistenthost.Transport{}}} {
		reader, ok := any(conv).(ports.ChatCompactionScopeReader)
		if !ok {
			t.Fatal("missing scope reader")
		}
		_, err := reader.CompactionScope(t.Context())
		if !errors.Is(err, ports.ErrChatCompactionScopeNoHost) {
			t.Fatal(err)
		}
	}
}
