package agentaccounts

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type timeoutEvidenceProvider struct{ *evidenceCodex }

func (*timeoutEvidenceProvider) Identity(ctx context.Context, _ string) (domain.AgentAccountIdentityCheck, error) {
	<-ctx.Done()
	return domain.AgentAccountIdentityCheck{}, ctx.Err()
}
func TestMissingEvidenceProbeTimeoutIsUnavailable(t *testing.T) {
	root := filepath.Join(t.TempDir(), "accounts")
	home := filepath.Join(root, "codex", "saved")
	if e := ensureAccountProfile(root, home); e != nil {
		t.Fatal(e)
	}
	p := &timeoutEvidenceProvider{&evidenceCodex{fakeClaude: &fakeClaude{}}}
	s := New(&memStore{}, []ports.AgentAccountProviderCLI{p}, root, nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	if e := s.checkLaunchCredentialStore(ctx, domain.AgentAccount{ID: "saved", Provider: domain.AgentAccountProviderCodex, Home: home}); !errors.Is(e, ports.ErrAgentAccountStorageUnavailable) {
		t.Fatalf("timeout was asserted as store unsupported: %v", e)
	}
}
func TestNonLaunchWarningIsDeduplicatedPerAccountAndFailure(t *testing.T) {
	root := filepath.Join(t.TempDir(), "accounts")
	home := filepath.Join(root, "codex", "saved")
	var out bytes.Buffer
	store := &memStore{accounts: []domain.AgentAccount{{ID: "saved", Provider: domain.AgentAccountProviderCodex, Home: home}}, defaults: map[domain.AgentAccountProvider]string{domain.AgentAccountProviderCodex: "saved"}, bindings: map[domain.SessionID]string{}}
	p := &evidenceCodex{fakeClaude: &fakeClaude{}}
	s := New(&diagnosticBindingStore{store}, []ports.AgentAccountProviderCLI{p, &fakeClaude{}}, root, nil, slog.New(slog.NewTextHandler(&out, nil)))
	for range 3 {
		if _, e := s.SessionLaunchEnv(context.Background(), "session", domain.HarnessClaudeCode); e != nil {
			t.Fatal(e)
		}
	}
	if strings.Count(out.String(), "non-launch account unavailable") != 1 {
		t.Fatal("same inactive account repeats warning on every launch")
	}
}

type diagnosticBindingStore struct{ *memStore }

func (*diagnosticBindingStore) GetSessionAgentAccount(context.Context, domain.SessionID, domain.AgentAccountProvider) (string, bool, error) {
	return "", false, nil
}
func (*diagnosticBindingStore) BindSessionAgentAccount(context.Context, domain.SessionID, domain.AgentAccountProvider, string, time.Time) error {
	return nil
}
