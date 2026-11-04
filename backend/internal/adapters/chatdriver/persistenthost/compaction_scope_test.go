package persistenthost

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestCompactionScopeReportsOriginalHostAndProvider(t *testing.T) {
	home := t.TempDir()
	link := filepath.Join(t.TempDir(), "home")
	if err := os.Symlink(home, link); err != nil {
		t.Fatal(err)
	}
	cfg := Config{SessionID: "scope-test", DataDir: t.TempDir(), Workdir: t.TempDir(), Env: append(os.Environ(), "AO_CHAT_HOST_PROVIDER_HELPER=1", "CODEX_HOME="+link), Argv: []string{os.Args[0], "-test.run=TestProviderHelper"}}
	done := make(chan error, 1)
	go func() { done <- Run(context.Background(), cfg) }()
	t.Cleanup(func() { _ = Shutdown(context.Background(), cfg.DataDir, cfg.SessionID); <-done })
	descriptor := awaitDescriptor(t, cfg.DataDir, cfg.SessionID)
	transport, err := attach(context.Background(), descriptor, false)
	if err != nil {
		t.Fatal(err)
	}
	defer transport.Stdin.Close()
	pid := requestProviderPID(t, transport, 1, "pid")
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), link); err != nil {
		t.Fatal(err)
	}
	scope, err := transport.Delivery.CompactionScope(context.Background())
	if err != nil || scope.NativeHome != home {
		t.Fatalf("launch home changed: %+v, %v", scope, err)
	}
	if runtime.GOOS == "linux" {
		p := scope.Processes
		if p == nil || p.HostPID != descriptor.PID || p.LeaderPID != pid || p.ProviderPGID != pid || p.HostBirth == "" || p.LeaderBirth == "" || p.BootID == "" || p.PIDNamespace == "" {
			t.Fatalf("missing original identity: %+v", p)
		}
	} else if scope.Processes != nil {
		t.Fatal("non-Linux host reported process identity")
	}
}

func TestCompactionScopeOlderHostReturnsEmpty(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var request hello
		_ = json.NewDecoder(conn).Decode(&request)
		_ = json.NewEncoder(conn).Encode(helloResponse{Error: "unknown chat host action"})
	}()
	client := &DeliveryClient{descriptor: Descriptor{Address: listener.Addr().String()}}
	scope, err := client.CompactionScope(context.Background())
	<-done
	if err != nil || scope.NativeHome != "" || scope.Processes != nil {
		t.Fatalf("old host response = %+v, %v", scope, err)
	}
}

func TestCompactionScopeUsesACPProviderHome(t *testing.T) {
	for _, provider := range []domain.AgentHarness{domain.HarnessOpenCode, domain.HarnessOMP} {
		for _, valid := range []bool{true, false} {
			name := "valid"
			if !valid {
				name = "invalid"
			}
			t.Run(string(provider)+"/"+name, func(t *testing.T) {
				home := t.TempDir()
				native := filepath.Join(home, "provider-root")
				if valid {
					if err := os.Mkdir(native, 0700); err != nil {
						t.Fatal(err)
					}
				}
				claude := filepath.Join(home, ".claude")
				if err := os.Mkdir(claude, 0700); err != nil {
					t.Fatal(err)
				}
				key := "OPENCODE_DATA_DIR"
				if provider == domain.HarnessOMP {
					key = "PI_CODING_AGENT_DIR"
				}
				cfg := Config{SessionID: "acp-home", DataDir: t.TempDir(), Workdir: home, Protocol: ProtocolACP,
					Env:  append(os.Environ(), "AO_CHAT_HOST_PROVIDER_HELPER=1", "AO_CHAT_HOST_ACP_HELPER=1", "AO_CHAT_HARNESS="+string(provider), "HOME="+home, key+"="+native, "CLAUDE_CONFIG_DIR="+claude),
					Argv: []string{os.Args[0], "-test.run=TestProviderHelper"}}
				done := make(chan error, 1)
				go func() { done <- Run(context.Background(), cfg) }()
				t.Cleanup(func() { _ = Shutdown(context.Background(), cfg.DataDir, cfg.SessionID); <-done })
				descriptor := awaitDescriptor(t, cfg.DataDir, cfg.SessionID)
				client := &DeliveryClient{descriptor: descriptor}
				scope, err := client.CompactionScope(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if valid {
					if scope.NativeHome != native || scope.NativeHomeError != "" {
						t.Fatalf("ACP provider home: %+v", scope)
					}
				} else if scope.NativeHome != "" || scope.NativeHomeError == "" {
					t.Fatalf("invalid ACP root fell back to Claude: %+v", scope)
				}
			})
		}
	}
}
