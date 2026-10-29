package persistenthost

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"
)

func TestShutdownReturnsDescriptorPermissionErrorOnLinux(t *testing.T) {
	for _, releaseLock := range []bool{false, true} {
		name := "lock retained"
		if releaseLock {
			name = "lock released"
		}
		t.Run(name, func(t *testing.T) {
			dataDir := t.TempDir()
			const sessionID = "permission-shutdown"
			release, err := acquireHostLock(dataDir, sessionID)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(release)
			address, accepted := silentHandshakePeer(t)
			if err := writeDescriptor(dataDir, Descriptor{
				Version: ProtocolVersion, SessionID: sessionID,
				Address: address, Token: "token", PID: os.Getpid(),
			}); err != nil {
				t.Fatal(err)
			}
			path, err := descriptorPath(dataDir, sessionID)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			result := make(chan error, 1)
			go func() { result <- Shutdown(ctx, dataDir, sessionID) }()
			conn := awaitSilentHandshake(t, accepted)
			defer func() { _ = conn.Close() }()
			// The initial read and request succeeded. Make only the read after the ACK
			// fail, keeping the same descriptor whether or not ownership is released.
			if err := os.Chmod(path, 0); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
			if _, err := readDescriptor(dataDir, sessionID); !errors.Is(err, os.ErrPermission) {
				t.Fatalf("unreadable descriptor fixture: %v, want permission denied", err)
			}
			if releaseLock {
				release()
			}
			if err := json.NewEncoder(conn).Encode(helloResponse{OK: true}); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-result:
				var pathErr *os.PathError
				if !errors.Is(err, os.ErrPermission) || !errors.As(err, &pathErr) || pathErr.Path != path {
					t.Fatalf("shutdown = %v, want descriptor permission error", err)
				}
				if errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
					t.Fatalf("shutdown waited for its deadline instead of returning permission error: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("shutdown did not return permission error")
			}
			lock, _ := lockPath(dataDir, sessionID)
			_, lockErr := os.Stat(lock)
			if (releaseLock && !errors.Is(lockErr, os.ErrNotExist)) || (!releaseLock && lockErr != nil) {
				t.Fatalf("shutdown changed ownership lock: released=%v, err=%v", releaseLock, lockErr)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("shutdown removed unreadable descriptor: %v", err)
			}
		})
	}
}
