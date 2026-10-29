package agentaccounts

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type prepareFailureAfterLogin struct {
	*fakeClaude
	calls int
}

func (p *prepareFailureAfterLogin) Prepare(string) error {
	p.calls++
	if p.calls > 1 {
		return errors.New("/fake/private/account: fake-secret-marker")
	}
	return nil
}

func TestVerifyLoginPrepareFailureRefusesWithoutLeaking(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("new managed login is Linux-only")
	}
	var output bytes.Buffer
	provider := &prepareFailureAfterLogin{fakeClaude: &fakeClaude{signedInAs: map[string]string{}}}
	root := filepath.Join(t.TempDir(), "accounts")
	store := &memStore{}
	svc := New(store, []ports.AgentAccountProviderCLI{provider}, root, fakeTerminals{}, slog.New(slog.NewTextHandler(&output, nil)))
	start, err := svc.StartLogin(context.Background(), domain.AgentAccountProviderClaude)
	if err != nil {
		t.Fatal(err)
	}
	provider.signedInAs[filepath.Join(root, "claude", start.OperationID)] = "fake@example.invalid"
	status, _, err := svc.VerifyLogin(context.Background(), start.OperationID)
	if !errors.Is(err, ports.ErrAgentAccountStorageUnavailable) || status != "" || len(store.accounts) != 0 {
		t.Fatalf("verify = %q, %v", status, err)
	}
	log := output.String() + err.Error()
	if strings.Contains(log, "/fake/private/account") || strings.Contains(log, "fake-secret-marker") {
		t.Fatalf("prepare error leaked through log: %s", log)
	}
	if !strings.Contains(log, "account_storage_unavailable") {
		t.Fatalf("missing safe category: %s", log)
	}
}
