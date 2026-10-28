//go:build !linux

package agentaccounts

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestNewManagedLoginIsUnsupportedWithoutCreatingProfile(t *testing.T) {
	root := filepath.Join(t.TempDir(), "accounts")
	svc := New(&memStore{}, []ports.AgentAccountProviderCLI{&fakeClaude{}}, root, fakeTerminals{}, nil)
	_, err := svc.StartLogin(context.Background(), domain.AgentAccountProviderClaude)
	if !errors.Is(err, ports.ErrAgentAccountStorageUnsupported) {
		t.Fatalf("new managed login = %v, want unsupported", err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unsupported login created profile: %v", err)
	}
}
