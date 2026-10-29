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

func TestAccountExecutionProtectsProfileFromLifecycleMutation(t *testing.T) {
	for _, action := range []string{"remove", "logout", "login-again"} {
		t.Run(action, func(t *testing.T) {
			ctx := context.Background()
			root := filepath.Join(t.TempDir(), "accounts")
			provider := &fakeClaude{signedInAs: map[string]string{}}
			st := &memStore{defaults: map[domain.AgentAccountProvider]string{}, bindings: map[domain.SessionID]string{}}
			svc := New(st, []ports.AgentAccountProviderCLI{provider}, root, fakeTerminals{}, nil)
			login, err := svc.StartLogin(ctx, domain.AgentAccountProviderClaude)
			if err != nil {
				t.Fatal(err)
			}
			home := filepath.Join(root, "claude", login.OperationID)
			provider.signedInAs[home] = "fake@example.test"
			if _, _, err := svc.VerifyLogin(ctx, login.OperationID); err != nil {
				t.Fatal(err)
			}
			guard, ok := any(svc).(interface {
				AcquireAccountUses(context.Context, ...string) (func(), error)
			})
			if !ok {
				t.Fatal("account execution has no lifecycle guard")
			}
			release, err := guard.AcquireAccountUses(ctx, login.OperationID)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			switch action {
			case "remove":
				err = svc.Remove(ctx, login.OperationID)
			case "logout":
				err = svc.Logout(ctx, login.OperationID)
			case "login-again":
				_, err = svc.StartSignInAgain(ctx, login.OperationID)
			}
			if !errors.Is(err, ports.ErrAgentAccountInUse) {
				t.Fatalf("active launch allowed %s: %v", action, err)
			}
			if _, err := os.Lstat(home); err != nil {
				t.Fatal("active profile was removed", err)
			}
			if provider.signedInAs[home] != "fake@example.test" {
				t.Fatal("active launch identity changed")
			}
			a, err := svc.find(ctx, login.OperationID)
			if err != nil || a.DisabledAt != nil || a.RetiredAt != nil {
				t.Fatalf("refused mutation changed account: %+v %v", a, err)
			}
			release()
			if err := svc.Remove(ctx, login.OperationID); err != nil {
				t.Fatal("released profile could not be removed", err)
			}
		})
	}
}

type usageOwnerStore struct {
	*memStore
	owners []domain.AgentAccountUsageOwner
}

func (s *usageOwnerStore) ListAgentAccountUsageOwners(context.Context, string) ([]domain.AgentAccountUsageOwner, error) {
	return s.owners, nil
}

func TestLiveOwnerRefusesRemoveAndReloginWithoutChangingAccount(t *testing.T) {
	for _, action := range []string{"remove", "login-again"} {
		t.Run(action, func(t *testing.T) {
			ctx := context.Background()
			root := filepath.Join(t.TempDir(), "accounts")
			provider := &fakeClaude{signedInAs: map[string]string{}}
			st := &usageOwnerStore{memStore: &memStore{defaults: map[domain.AgentAccountProvider]string{}, bindings: map[domain.SessionID]string{}}}
			svc := New(st, []ports.AgentAccountProviderCLI{provider}, root, fakeTerminals{}, nil)
			login, err := svc.StartLogin(ctx, domain.AgentAccountProviderClaude)
			if err != nil {
				t.Fatal(err)
			}
			home := filepath.Join(root, "claude", login.OperationID)
			provider.signedInAs[home] = "fake@example.test"
			if _, _, err := svc.VerifyLogin(ctx, login.OperationID); err != nil {
				t.Fatal(err)
			}
			st.owners = []domain.AgentAccountUsageOwner{{Kind: "session", SessionID: "fake-1", SessionName: "Fake running worker", Harness: domain.HarnessClaudeCode, Blocking: true}}
			stopped := false
			svc.SetAgentStopper(func(context.Context, string) error { stopped = true; return nil })
			if action == "remove" {
				err = svc.Remove(ctx, login.OperationID)
			} else {
				_, err = svc.StartSignInAgain(ctx, login.OperationID)
			}
			var use *ports.AgentAccountInUseError
			if !errors.As(err, &use) || len(use.Owners) != 1 || stopped {
				t.Fatalf("live owner was not refused before effects: %v stopped=%t", err, stopped)
			}
			a, err := svc.find(ctx, login.OperationID)
			if err != nil || a.DisabledAt != nil || a.RetiredAt != nil {
				t.Fatalf("refused mutation disabled account: %+v %v", a, err)
			}
			if provider.signedInAs[home] != "fake@example.test" {
				t.Fatal("live owner was logged out")
			}
		})
	}
}

func TestContainedHandoffRejectsExternalHistoryBeforeEffects(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "accounts")
	cli := &fakeClaude{signedInAs: map[string]string{}}
	st := &memStore{defaults: map[domain.AgentAccountProvider]string{}, bindings: map[domain.SessionID]string{}}
	svc := New(st, []ports.AgentAccountProviderCLI{cli}, root, fakeTerminals{}, nil)
	login, err := svc.StartLogin(ctx, domain.AgentAccountProviderClaude)
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "claude", login.OperationID)
	cli.signedInAs[home] = "fake@example.test"
	if _, _, err = svc.VerifyLogin(ctx, login.OperationID); err != nil {
		t.Fatal(err)
	}
	validator, ok := any(svc).(interface {
		ValidateAccountHandoff(context.Context, domain.AgentHarness, string, string, string) error
	})
	if !ok {
		t.Fatal("no legacy handoff boundary")
	}
	if err = validator.ValidateAccountHandoff(ctx, domain.HarnessClaudeCode, "device-claude", login.OperationID, "external-thread"); !errors.Is(err, ports.ErrAgentAccountUnshared) {
		t.Fatalf("external history accepted: %v", err)
	}
	if err = validator.ValidateAccountHandoff(ctx, domain.HarnessClaudeCode, "device-claude", login.OperationID, ""); err != nil {
		t.Fatal("fresh conversation cannot move", err)
	}
	cli.signedInAs[home] = "wrong@example.test"
	if err = validator.ValidateAccountHandoff(ctx, domain.HarnessClaudeCode, login.OperationID, login.OperationID, ""); !errors.Is(err, ports.ErrAgentAccountSignedOut) {
		t.Fatalf("handoff used stale target identity: %v", err)
	}
}
func TestLegacyIdentityConflictNeverSavesSecondCredentialCopy(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "accounts")
	cli := &fakeClaude{signedInAs: map[string]string{}}
	st := &memStore{defaults: map[domain.AgentAccountProvider]string{}, bindings: map[domain.SessionID]string{}}
	svc := New(st, []ports.AgentAccountProviderCLI{cli}, root, fakeTerminals{}, nil)
	setter, ok := any(svc).(interface {
		SetLegacyIdentityCheck(func(context.Context, domain.AgentAccountProvider, string) (bool, error))
	})
	if !ok {
		t.Fatal("no legacy identity boundary")
	}
	setter.SetLegacyIdentityCheck(func(_ context.Context, p domain.AgentAccountProvider, email string) (bool, error) {
		return p == domain.AgentAccountProviderClaude && email == "fake@example.test", nil
	})
	login, err := svc.StartLogin(ctx, domain.AgentAccountProviderClaude)
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "claude", login.OperationID)
	cli.signedInAs[home] = "fake@example.test"
	for range 2 {
		_, _, err = svc.VerifyLogin(ctx, login.OperationID)
		if err == nil || err.Error() != "account_legacy_identity_conflict" {
			t.Fatalf("legacy conflict lost: %v", err)
		}
	}
	if len(st.accounts) != 0 {
		t.Fatal("duplicate credential owner saved")
	}
	if _, err = os.Lstat(home); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("new duplicate profile retained", err)
	}
}
