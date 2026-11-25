package persistenthost

import (
	"context"
	"errors"
	"os"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/processalive"
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

func TestProviderOwnershipRequiresConsistentPreviousBootRecords(t *testing.T) {
	now := time.Date(2026, 11, 18, 9, 48, 13, 0, time.UTC)
	stubBoot(t, "boot-now", now.Add(-time.Hour), nil)
	const sessionID = "provider-ownership"
	pid := os.Getpid() // A live PID may have been reused since the previous boot.
	for _, tc := range []struct {
		name, descriptorBoot, lock string
		spawnPID                   int
		want                       ProviderOwnership
	}{
		{name: "no records", want: ProviderOwnershipUnrecorded},
		{name: "confirmed previous boot", descriptorBoot: "boot-old", lock: strconv.Itoa(pid) + " boot-old", want: ProviderOwnershipPreviousBoot},
		{name: "same boot", descriptorBoot: "boot-now", want: ProviderOwnershipUncertain},
		{name: "current lock", descriptorBoot: "boot-old", lock: strconv.Itoa(pid) + " boot-now", want: ProviderOwnershipUncertain},
		{name: "current spawn", descriptorBoot: "boot-old", spawnPID: pid, want: ProviderOwnershipUncertain},
		{name: "unattributed spawn", spawnPID: pid, want: ProviderOwnershipUncertain},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if tc.descriptorBoot != "" {
				if err := writeDescriptor(root, Descriptor{Version: ProtocolVersion, SessionID: sessionID, Address: "127.0.0.1:1",
					Token: "fixture", PID: pid, StartedAt: now, BootID: tc.descriptorBoot}); err != nil {
					t.Fatal(err)
				}
			}
			if tc.lock != "" {
				path, err := lockPath(root, sessionID)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(tc.lock), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.spawnPID != 0 {
				if err := recordSpawn(root, sessionID, tc.spawnPID); err != nil {
					t.Fatal(err)
				}
			}
			if got := ProbeProviderOwnership(root, sessionID); got != tc.want {
				t.Fatalf("provider ownership = %v, want %v", got, tc.want)
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

func TestEpochFromPreviousBootNeedsBootProof(t *testing.T) {
	booted := time.Date(2026, 11, 18, 9, 48, 13, 0, time.UTC)
	stubBoot(t, "boot-now", booted, nil)
	for _, tc := range []struct {
		epoch    string
		previous bool
	}{
		{"host-42-" + strconv.FormatInt(booted.Add(time.Minute).UnixNano(), 10) + "-boot-old", true},
		{"host-42-" + strconv.FormatInt(booted.Add(time.Minute).UnixNano(), 10) + "-boot-now", false},
		{"host-42-" + strconv.FormatInt(booted.Add(-time.Hour).UnixNano(), 10), true},   // u3 epoch, started before this boot
		{"host-42-" + strconv.FormatInt(booted.Add(time.Minute).UnixNano(), 10), false}, // u3 epoch, this boot
		{"fake-native-epoch", false},
		{"host-x-1", false},
	} {
		if got := EpochFromPreviousBoot(tc.epoch); got != tc.previous {
			t.Errorf("EpochFromPreviousBoot(%q) = %v, want %v", tc.epoch, got, tc.previous)
		}
	}
}

// A host killed in this boot (SIGKILL/OOM) leaves its provider running in its
// own process group. The next host may take over, but the killed host's epoch
// must not count as proof that its work ended (SDR-PR1 review P1).
func TestKilledHostEpochIsNotProofWhileItsProviderRuns(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("boot identity is recorded on Linux only")
	}
	cfg := Config{SessionID: "killed-host", DataDir: t.TempDir(), Workdir: t.TempDir(),
		Env:  append(os.Environ(), "AO_CHAT_HOST_PROVIDER_HELPER=1", "AO_CHAT_HOST_IGNORE_STDIN=1"),
		Argv: []string{os.Args[0], "-test.run=TestProviderHelper"}}
	first, err := ConnectOrStart(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = Shutdown(context.Background(), cfg.DataDir, cfg.SessionID) })
	provider := requestProviderPID(t, first, 1, "pid")
	t.Cleanup(func() { killPID(provider) })
	old, err := readDescriptor(cfg.DataDir, cfg.SessionID)
	if err != nil || old.BootID == "" {
		t.Fatalf("host descriptor = %+v, %v", old, err)
	}
	oldEpoch := deliveryEpoch(old)
	killPID(old.PID) // SIGKILL, as the OOM killer would
	// Wait for the detached host's parent to reap it before attempting takeover.
	for deadline := time.Now().Add(5 * time.Second); !processalive.Exited(old.PID); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("killed host PID was not reaped")
		}
	}
	if !processalive.Alive(provider) {
		t.Fatal("the killed host's provider stopped")
	}
	next, err := ConnectOrStart(context.Background(), cfg)
	if err != nil {
		t.Fatalf("next host: %v", err)
	}
	_ = next.Stdin.Close()
	if next.Delivery.ProviderEpoch() == oldEpoch {
		t.Fatal("next host reused the killed host's epoch")
	}
	if EpochFromPreviousBoot(oldEpoch) {
		t.Fatalf("killed host's epoch %q judged from a previous boot while its provider %d still runs", oldEpoch, provider)
	}
}

func killPID(pid int) {
	if process, err := os.FindProcess(pid); err == nil {
		_ = process.Kill()
	}
}
