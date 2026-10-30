package session

import (
	"context"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type retryAccountCommander struct {
	*fakeCommander
	calls     int
	confirmed bool
}

func (m *retryAccountCommander) RetryAccountRecovery(_ context.Context, _ domain.SessionID) (bool, error) {
	m.calls++
	return m.confirmed, nil
}
func TestAccountRecoveryServiceRejectsRemoteBeforeManager(t *testing.T) {
	manager := &retryAccountCommander{fakeCommander: &fakeCommander{}, confirmed: true}
	svc := &Service{manager: manager}
	if _, err := svc.RetryAccountRecovery(ports.WithRemoteAccountRequest(context.Background()), "fake-session"); err == nil || manager.calls != 0 {
		t.Fatalf("remote recovery reached manager: err=%v calls=%d", err, manager.calls)
	}
	for _, want := range []bool{false, true} {
		manager.confirmed = want
		confirmed, err := svc.RetryAccountRecovery(context.Background(), "fake-session")
		if err != nil || confirmed != want {
			t.Fatalf("confirmation lost: %v %v", confirmed, err)
		}
	}
}
