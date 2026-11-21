package mcpserver

import (
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

func TestDaemonToolCallRequiresVerifiedReadyEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name, ready, failure string
		pid                  int
	}{
		{"verified", "ready", "", os.Getpid()},
		{"other process", "ready", "does not match run-file", os.Getpid() + 1},
		{"starting", "starting", "readyz", os.Getpid()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := setConfigEnv(t)
			var calls atomic.Int32
			srv := newProbeFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.WriteHeader(http.StatusNoContent)
			}), tc.pid, tc.ready)
			writeRunFileFor(t, cfg, srv)
			err := NewDaemonClient().PostJSON(t.Context(), "fixture", map[string]string{"body": "fixture"}, nil)
			if tc.failure == "" {
				if err != nil || calls.Load() != 1 {
					t.Fatalf("verified daemon calls=%d, err=%v", calls.Load(), err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.failure) || calls.Load() != 0 {
				t.Fatalf("unverified daemon calls=%d, err=%v", calls.Load(), err)
			}
		})
	}
}
