package agentaccounts

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

type recoveryCatalogStore struct {
	memStore
	lists int
}

func (s *recoveryCatalogStore) ListAgentAccounts(ctx context.Context) ([]domain.AgentAccount, error) {
	s.lists++
	return s.memStore.ListAgentAccounts(ctx)
}
func TestRecoveryCancelledHistoryReadsCatalogOncePerSweep(t *testing.T) {
	st := &recoveryCatalogStore{memStore: memStore{operations: map[string]domain.AgentAccountOperation{}}}
	for i := range 40 {
		id := fmt.Sprintf("cancelled-%d", i)
		st.operations[id] = domain.AgentAccountOperation{ID: id, AccountID: id, Provider: domain.AgentAccountProviderClaude, Action: "login", Phase: "complete", Outcome: "cancelled"}
	}
	s := New(st, nil, filepath.Join(t.TempDir(), "accounts"), nil, nil)
	for want := 1; want <= 2; want++ {
		if err := s.Recover(context.Background()); err != nil {
			t.Fatal(err)
		}
		if st.lists != want {
			t.Fatalf("catalog reads=%d want %d for %d sweeps of 40 completed operations", st.lists, want, want)
		}
	}
}
