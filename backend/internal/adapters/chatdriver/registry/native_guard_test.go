package registry

import (
	"context"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"testing"
)

type passNativeGuard struct{}

func (passNativeGuard) Run(_ context.Context, _ domain.AgentHarness, _ ports.ChatResumeConfig, call func() error) error {
	return call()
}
func TestNativeGuardPreservesDriverCapabilities(t *testing.T) {
	original := Build(nil, nil)
	for _, h := range original.Harnesses() {
		t.Run(string(h), func(t *testing.T) {
			raw, _ := original.Driver(h)
			wrapped := guardDriver(raw, passNativeGuard{})
			_, a := raw.(ports.ChatDriverReconnector)
			_, b := wrapped.(ports.ChatDriverReconnector)
			if a != b {
				t.Fatal("Reconnect capability changed")
			}
			_, a = raw.(ports.ChatIsolatedForker)
			_, b = wrapped.(ports.ChatIsolatedForker)
			if a != b {
				t.Fatal("Fork capability changed")
			}
			_, a = raw.(ports.ChatInstallChecker)
			_, b = wrapped.(ports.ChatInstallChecker)
			if a != b {
				t.Fatal("Install capability changed")
			}
			_, a = raw.(ports.ChatPermissionModeChecker)
			_, b = wrapped.(ports.ChatPermissionModeChecker)
			if a != b {
				t.Fatal("Permissions capability changed")
			}
			_, a = raw.(ports.NativeCheckpointVerifier)
			_, b = wrapped.(ports.NativeCheckpointVerifier)
			if a != b {
				t.Fatal("Checkpoint capability changed")
			}
		})
	}
}
