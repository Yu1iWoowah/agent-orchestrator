package agentaccounts

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type resetErrorProvider struct {
	*evidenceCodex
	err error
}

func (p *resetErrorProvider) ConsumeResetCredit(context.Context, string, string) (domain.CodexResetCreditOutcome, error) {
	return domain.CodexResetCreditReset, p.err
}

func TestResetCreditErrorPreservesOnlySafeCategories(t *testing.T) {
	for _, category := range []error{ports.ErrAgentAccountCredentialStoreUnsupported, context.DeadlineExceeded, errAccountProviderUnavailable} {
		t.Run(category.Error(), func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "accounts")
			home := filepath.Join(root, "codex", "saved")
			if err := os.MkdirAll(home, 0o700); err != nil {
				t.Fatal(err)
			}
			cause := fmt.Errorf("fake-private-marker/private/profile: %w", category)
			if errors.Is(category, errAccountProviderUnavailable) {
				cause = errors.New("fake-private-marker/private/profile: raw provider RPC failure")
			}
			provider := &resetErrorProvider{evidenceCodex: &evidenceCodex{fakeClaude: &fakeClaude{}}, err: cause}
			store := &memStore{accounts: []domain.AgentAccount{{ID: "saved", Provider: domain.AgentAccountProviderCodex, Home: home}}}
			svc := New(store, []ports.AgentAccountProviderCLI{provider}, root, nil, nil)
			outcome, err := svc.ConsumeResetCredit(context.Background(), "saved", "fake-once")
			if outcome != "" || !errors.Is(err, category) {
				t.Fatal("failed reset did not preserve its safe category")
			}
			if strings.Contains(err.Error(), "fake-private-marker") || strings.Contains(err.Error(), "/private/") || errors.Unwrap(err) != nil {
				t.Fatal("reset error retained provider details or cause")
			}
		})
	}
}
