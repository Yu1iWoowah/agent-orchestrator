package acp

import (
	"errors"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestCompactionScopeWithoutPersistentHost(t *testing.T) {
	for _, conv := range []*conversation{{}, {proc: &process{}}} {
		_, err := conv.CompactionScope(t.Context())
		if !errors.Is(err, ports.ErrChatCompactionScopeNoHost) {
			t.Fatalf("missing host scope=%v", err)
		}
	}
}
