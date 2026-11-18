package persistenthost

import (
	"context"
	"errors"
	"os"
	"runtime"
	"strconv"
	"testing"
	"time"
)

func stubBoot(t *testing.T, id string, booted time.Time, idErr error) {
	t.Helper()
	oldID, oldTime := readBootID, readBootTime
	t.Cleanup(func() { readBootID, readBootTime = oldID, oldTime })
	readBootID = func() (string, error) { return id, idErr }
	readBootTime = func() (time.Time, error) { return booted, nil }
}

func TestHostFromPreviousBootNeedsPositiveProof(t *testing.T) {
	booted := time.Date(2026, 11, 18, 9, 48, 13, 0, time.UTC)
	for _, tc := range []struct {
		name      string
		recorded  string
		startedAt time.Time
		idErr     error
		previous  bool
	}{
		{"other boot id", "boot-old", time.Time{}, nil, true},
		{"same boot id", "boot-now", time.Time{}, nil, false},
		{"boot id unreadable", "boot-old", time.Time{}, errors.New("no proc"), false},
		{"u3 record started before this boot", "", booted.Add(-time.Hour), nil, true},
		{"u3 record started after this boot", "", booted.Add(time.Minute), nil, false},
		{"u3 record without start time", "", time.Time{}, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubBoot(t, "boot-now", booted, tc.idErr)
			if got := hostFromPreviousBoot(tc.recorded, tc.startedAt); got != tc.previous {
				t.Fatalf("hostFromPreviousBoot = %v, want %v", got, tc.previous)
			}
		})
	}
}

// A reboot leaves host.json and host.lock naming a PID that is now an
// unrelated live process and an address that refuses: the records are proven
// stale by boot identity, so the lock is reclaimed and a new host starts
// (SDR-PR1 AC2). Without that proof the same records stay inconclusive.
func TestRebootLeftoverHostRecordsAreReclaimed(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("boot identity is recorded on Linux only")
	}
	current := currentBootID()
	if current == "" {
		t.Skip("boot id unavailable")
	}
	livePID := os.Getpid() // alive, and certainly not a chat host
	for _, tc := range []struct {
		name          string
		descriptorID  string
		startedAt     time.Time
		lock          string
		reconnectOnly bool
		want          error // nil: a new host starts
	}{
		{"previous boot id", "00000000-0000-0000-0000-000000000000", time.Now(), strconv.Itoa(livePID) + " 00000000-0000-0000-0000-000000000000\n", false, nil},
		{"previous boot id, reconnect only", "00000000-0000-0000-0000-000000000000", time.Now(), strconv.Itoa(livePID) + " 00000000-0000-0000-0000-000000000000\n", true, ErrNotRunning},
		{"u3 records started before this boot", "", time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), strconv.Itoa(livePID) + "\n", false, nil},
		{"same boot", current, time.Now(), strconv.Itoa(livePID) + " " + current + "\n", false, ErrOwnershipInconclusive},
		{"u3 records started in this boot", "", time.Now(), strconv.Itoa(livePID) + "\n", false, ErrOwnershipInconclusive},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{SessionID: "rebooted", DataDir: t.TempDir(), Workdir: t.TempDir(), ReconnectOnly: tc.reconnectOnly,
				Env:  append(os.Environ(), "AO_CHAT_HOST_PROVIDER_HELPER=1"),
				Argv: []string{os.Args[0], "-test.run=TestProviderHelper"}}
			if err := writeDescriptor(cfg.DataDir, Descriptor{Version: ProtocolVersion, SessionID: cfg.SessionID, Address: "127.0.0.1:1",
				Token: "stale", PID: livePID, StartedAt: tc.startedAt, BootID: tc.descriptorID}); err != nil {
				t.Fatal(err)
			}
			lock, _ := lockPath(cfg.DataDir, cfg.SessionID)
			if err := os.WriteFile(lock, []byte(tc.lock), 0o600); err != nil {
				t.Fatal(err)
			}
			transport, err := ConnectOrStart(context.Background(), cfg)
			if tc.want != nil {
				if !errors.Is(err, tc.want) {
					t.Fatalf("ConnectOrStart = %v, want %v", err, tc.want)
				}
				if b, _ := os.ReadFile(lock); string(b) != tc.lock {
					t.Fatalf("lock rewritten without proof: %q", b)
				}
				return
			}
			if err != nil {
				t.Fatalf("ConnectOrStart over reboot leftovers: %v", err)
			}
			t.Cleanup(func() { _ = Shutdown(context.Background(), cfg.DataDir, cfg.SessionID) })
			_ = transport.Stdin.Close()
			d, err := readDescriptor(cfg.DataDir, cfg.SessionID)
			if err != nil || d.PID == livePID || d.BootID != current {
				t.Fatalf("new host descriptor = %+v, %v", d, err)
			}
		})
	}
}
