package persistenthost

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
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
