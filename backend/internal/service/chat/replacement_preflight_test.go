package chat_test

import (
	"context"
	"errors"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
)

func TestReplacementChatPreflightRequiresResumeWithoutStartingProvider(t *testing.T) {
	for _, resume := range []bool{false, true} {
		caps := productionCaps()
		caps[ports.ChatCapabilityResume] = resume
		driver := fakeDriver{caps: caps, start: func(ports.ChatStartConfig) (ports.ChatConversation, error) {
			t.Fatal("preflight started the provider")
			return nil, nil
		}}
		svc := chatsvc.New(chatsvc.Options{Drivers: fakeRegistry{driver: driver}})
		err := svc.PreflightReplacementChat(context.Background(), domain.HarnessClaudeCode, ports.PermissionModeAuto)
		if resume && err != nil || !resume && !errors.Is(err, ports.ErrChatUnsupported) {
			t.Fatalf("resume=%t preflight=%v", resume, err)
		}
	}
}
