package agentaccounts

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestLoginRejectsWritableAncestorBeforeProviderAndCanRetry(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "private-account-parent")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o775); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(parent, "accounts")
	provider := &fakeClaude{signedInAs: map[string]string{}}
	svc := New(&memStore{}, []ports.AgentAccountProviderCLI{provider}, root, fakeTerminals{}, nil)
	if _, err := svc.StartLogin(context.Background(), domain.AgentAccountProviderClaude); err == nil {
		t.Fatal("login opened a provider terminal under a group-writable ancestor")
	} else if strings.Contains(err.Error(), parent) {
		t.Fatal("storage error disclosed a path")
	}
	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		t.Fatalf("unsafe login changed storage: %v", err)
	}
	if err := os.Chmod(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.StartLogin(context.Background(), domain.AgentAccountProviderClaude); err != nil {
		t.Fatalf("explicit retry after repair: %v", err)
	}
}

func TestLoginRejectsSymlinkedRootWithoutChangingTarget(t *testing.T) {
	outside := t.TempDir()
	root := filepath.Join(t.TempDir(), "accounts")
	if err := os.Symlink(outside, root); err != nil {
		t.Fatal(err)
	}
	svc := New(&memStore{}, []ports.AgentAccountProviderCLI{&fakeClaude{}}, root, fakeTerminals{}, nil)
	if _, err := svc.StartLogin(context.Background(), domain.AgentAccountProviderClaude); err == nil {
		t.Fatal("login followed symlinked root")
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("symlink target changed: %v %v", entries, err)
	}
}

func TestSavedAccountStorageIsCheckedBeforeIdentity(t *testing.T) {
	root := filepath.Join(t.TempDir(), "accounts")
	home := filepath.Join(root, "claude", "saved")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(home, 0o755); err != nil {
		t.Fatal(err)
	}
	provider := &profileCheckingClaude{fakeClaude: &fakeClaude{signedInAs: map[string]string{home: "test@example.invalid"}}}
	store := &memStore{accounts: []domain.AgentAccount{{ID: "saved", Provider: domain.AgentAccountProviderClaude, Home: home}}}
	svc := New(store, []ports.AgentAccountProviderCLI{provider}, root, fakeTerminals{}, nil)
	views, err := svc.List(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	for _, view := range views {
		if view.Account.ID == "saved" && (view.SignedIn || view.IdentityErr == "") {
			t.Fatal("unsafe saved profile was reported signed in")
		}
	}
	if provider.openedSaved {
		t.Fatal("identity probe opened unsafe profile")
	}
	info, err := os.Stat(home)
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("existing permissions changed: %v %v", info, err)
	}
}

type profileCheckingClaude struct {
	*fakeClaude
	openedSaved bool
}

func (p *profileCheckingClaude) Identity(ctx context.Context, home string) (domain.AgentAccountIdentityCheck, error) {
	if home != "" {
		p.openedSaved = true
	}
	return p.fakeClaude.Identity(ctx, home)
}

func TestLoginCreatesOnlyPrivateDirectories(t *testing.T) {
	root := filepath.Join(t.TempDir(), "accounts")
	svc := New(&memStore{}, []ports.AgentAccountProviderCLI{&fakeClaude{}}, root, fakeTerminals{}, nil)
	start, err := svc.StartLogin(context.Background(), domain.AgentAccountProviderClaude)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{root, filepath.Join(root, "claude"), filepath.Join(root, "claude", start.OperationID)} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("not private: %v %v", info, err)
		}
	}
}

func TestLoginRejectsNonPrivateExistingRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	svc := New(&memStore{}, []ports.AgentAccountProviderCLI{&fakeClaude{}}, root, fakeTerminals{}, nil)
	if _, err := svc.StartLogin(context.Background(), domain.AgentAccountProviderClaude); err == nil {
		t.Fatal("non-private root accepted")
	}
	info, err := os.Stat(root)
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("existing root mode changed: %v %v", info, err)
	}
}

func TestSignInRejectsUnsafeCredentialMetadata(t *testing.T) {
	for _, kind := range []string{"readable", "symlink", "hardlink"} {
		t.Run(kind, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "accounts")
			home := filepath.Join(root, "claude", "saved")
			if err := os.MkdirAll(home, 0o700); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(home, ".credentials.json")
			outside := filepath.Join(t.TempDir(), "fake-credential")
			if err := os.WriteFile(outside, []byte("fake-secret-marker"), 0o600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "readable":
				if err := os.WriteFile(target, []byte("fake-secret-marker"), 0o644); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(outside, target); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(outside, target); err != nil {
					t.Fatal(err)
				}
			}
			store := &memStore{accounts: []domain.AgentAccount{{ID: "saved", Provider: domain.AgentAccountProviderClaude, Home: home}}}
			svc := New(store, []ports.AgentAccountProviderCLI{&fakeClaude{}}, root, fakeTerminals{}, nil)
			if _, err := svc.StartSignInAgain(context.Background(), "saved"); err == nil {
				t.Fatal("unsafe credential reached sign-in terminal")
			}
			data, err := os.ReadFile(outside)
			if err != nil || string(data) != "fake-secret-marker" {
				t.Fatalf("external fake credential changed: %v", err)
			}
		})
	}
}

func TestUnsafeProfileIsNotLoggedOutOrRemoved(t *testing.T) {
	root := filepath.Join(t.TempDir(), "accounts")
	home := filepath.Join(root, "claude", "saved")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(home, 0o755); err != nil {
		t.Fatal(err)
	}
	provider := &fakeClaude{signedInAs: map[string]string{home: "test@example.invalid"}}
	store := &memStore{accounts: []domain.AgentAccount{{ID: "saved", Provider: domain.AgentAccountProviderClaude, Home: home}}}
	svc := New(store, []ports.AgentAccountProviderCLI{provider}, root, fakeTerminals{}, nil)
	if err := svc.Remove(context.Background(), "saved"); err == nil {
		t.Fatal("unsafe profile removal succeeded")
	}
	if provider.signedInAs[home] == "" {
		t.Fatal("unsafe profile reached provider logout")
	}
	if _, err := os.Stat(home); err != nil {
		t.Fatalf("unsafe profile was deleted: %v", err)
	}
}

func TestValidateAccountReturnsStorageFailureBeforeProvider(t *testing.T) {
	root := filepath.Join(t.TempDir(), "accounts")
	home := filepath.Join(root, "claude", "saved")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(home, 0o755); err != nil {
		t.Fatal(err)
	}
	provider := &profileCheckingClaude{fakeClaude: &fakeClaude{signedInAs: map[string]string{home: "test@example.invalid"}}}
	store := &memStore{accounts: []domain.AgentAccount{{ID: "saved", Provider: domain.AgentAccountProviderClaude, Home: home}}}
	svc := New(store, []ports.AgentAccountProviderCLI{provider}, root, fakeTerminals{}, nil)
	if err := svc.ValidateAccount(context.Background(), domain.HarnessClaudeCode, "saved"); !errors.Is(err, ports.ErrAgentAccountStorageUnsafe) {
		t.Fatalf("storage failure hidden as another error: %v", err)
	}
	if provider.openedSaved {
		t.Fatal("validation opened unsafe provider")
	}
}

func TestAccountCannotUseTheSharedRootAsItsHome(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	store := &memStore{accounts: []domain.AgentAccount{{ID: "saved", Provider: domain.AgentAccountProviderClaude, Home: root}}}
	svc := New(store, []ports.AgentAccountProviderCLI{&fakeClaude{}}, root, fakeTerminals{}, nil)
	if _, err := svc.StartSignInAgain(context.Background(), "saved"); err == nil {
		t.Fatal("shared account root used as a private account home")
	}
}
