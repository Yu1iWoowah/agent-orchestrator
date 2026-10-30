package persistenthost

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"reflect"
	"syscall"
	"testing"
)

func TestReplacementShutdownUsesPositiveJobProof(t *testing.T) {
	for _, tc := range []struct {
		name       string
		platform   string
		cleanupErr error
		waitErr    error
		treeErr    error
		confirmed  bool
		order      []string
	}{
		{name: "Windows reaped handle", platform: "windows", cleanupErr: syscall.EINVAL, confirmed: true, order: []string{"tree", "wait"}},
		{name: "Windows arbitrary kill failure", platform: "windows", cleanupErr: errors.New("process access denied"), confirmed: true, order: []string{"tree", "wait"}},
		{name: "Windows wait failure after job proof", platform: "windows", waitErr: errors.New("process wait handle released"), confirmed: true, order: []string{"tree", "wait"}},
		{name: "Windows active job", platform: "windows", treeErr: ErrOwnershipInconclusive, order: []string{"tree"}},
		{name: "Windows job close failure", platform: "windows", cleanupErr: syscall.EINVAL, treeErr: errors.New("job close failed"), order: []string{"tree"}},
		{name: "Unix cleanup failure", platform: "linux", cleanupErr: syscall.EINVAL, order: []string{"wait", "tree"}},
		{name: "Unix wait failure", platform: "darwin", waitErr: errors.New("wait failed"), order: []string{"wait", "tree"}},
		{name: "Unix exit code", platform: "linux", waitErr: &exec.ExitError{}, confirmed: true, order: []string{"wait", "tree"}},
		{name: "Unix group still alive", platform: "linux", treeErr: ErrOwnershipInconclusive, order: []string{"wait", "tree"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var order []string
			response := confirmedProviderShutdown(tc.platform, tc.cleanupErr, func() error {
				order = append(order, "wait")
				return tc.waitErr
			}, func(verify bool) error {
				if !verify {
					t.Fatal("cleanup was not asked for a positive stop proof")
				}
				order = append(order, "tree")
				return tc.treeErr
			})
			if response.OK != tc.confirmed || response.ProviderStopped != tc.confirmed || !reflect.DeepEqual(order, tc.order) {
				t.Fatalf("confirmation=%+v cleanup order=%v; want confirmed=%t order=%v", response, order, tc.confirmed, tc.order)
			}
		})
	}
}

func TestReplacementShutdownRequiresExistingMatchingHost(t *testing.T) {
	ctx := context.Background()
	if err := ShutdownForReplacement(ctx, t.TempDir(), "missing", "expected-owner", false); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("missing descriptor was accepted as shutdown proof: %v", err)
	}
	cfg := Config{SessionID: "replacement-owner", DataDir: t.TempDir(), Workdir: t.TempDir(), Protocol: ProtocolACP, OwnershipFingerprint: "expected-owner",
		Env: append(os.Environ(), "AO_CHAT_HOST_PROVIDER_HELPER=1"), Argv: []string{os.Args[0], "-test.run=TestProviderHelper"}}
	transport, err := ConnectOrStart(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = Shutdown(context.Background(), cfg.DataDir, cfg.SessionID) })
	defer func() { _ = transport.Stdin.Close() }()
	pid := requestProviderPID(t, transport, 1, "pid")
	if err := ShutdownForReplacement(ctx, cfg.DataDir, cfg.SessionID, "other-owner", false); !errors.Is(err, ErrIncompatible) {
		t.Fatalf("mismatched owner was accepted: %v", err)
	}
	if requestProviderPID(t, transport, 2, "pid") != pid {
		t.Fatal("refused shutdown replaced the provider")
	}
}

func TestReplacementShutdownRejectsAcceptanceWithoutStopConfirmation(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	dir := t.TempDir()
	if err := writeDescriptor(dir, Descriptor{Version: ProtocolVersion, SessionID: "acceptance-only", Protocol: ProtocolACP, OwnershipFingerprint: "expected-owner", StopConfirmation: true, Address: listener.Addr().String(), PID: os.Getpid(), Token: "test-token"}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer func() { _ = conn.Close() }()
		var request hello
		if err := json.NewDecoder(conn).Decode(&request); err != nil {
			done <- err
			return
		}
		if request.Action != "shutdown-confirmed" {
			done <- errors.New("not a confirmed shutdown request")
			return
		}
		done <- json.NewEncoder(conn).Encode(helloResponse{OK: true})
	}()
	if err := ShutdownForReplacement(context.Background(), dir, "acceptance-only", "expected-owner", false); !errors.Is(err, ErrOwnershipInconclusive) {
		t.Fatalf("acceptance was mistaken for stopped provider: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestReplacementShutdownConfirmsCleanupBeforeReturning(t *testing.T) {
	ctx := context.Background()
	cfg := Config{SessionID: "replacement-confirmed", DataDir: t.TempDir(), Workdir: t.TempDir(), Protocol: ProtocolACP, OwnershipFingerprint: "expected-owner",
		Env: append(os.Environ(), "AO_CHAT_HOST_PROVIDER_HELPER=1", "AO_CHAT_HOST_DELAY_EXIT=1"), Argv: []string{os.Args[0], "-test.run=TestProviderHelper"}}
	transport, err := ConnectOrStart(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = Shutdown(context.Background(), cfg.DataDir, cfg.SessionID) })
	_ = requestProviderPID(t, transport, 1, "pid")
	_ = transport.Stdin.Close()
	if err := ShutdownForReplacement(ctx, cfg.DataDir, cfg.SessionID, cfg.OwnershipFingerprint, false); err != nil {
		t.Fatal(err)
	}
	if _, err := readDescriptor(cfg.DataDir, cfg.SessionID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("confirmed shutdown did not release host ownership: %v", err)
	}
}
