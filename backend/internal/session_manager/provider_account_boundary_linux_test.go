package sessionmanager

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agentaccount"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/agentaccounts"
)

type providerAccountBinding struct {
	session  domain.SessionID
	provider domain.AgentAccountProvider
}

type providerAccountStore struct {
	ports.AgentAccountStore
	accounts []domain.AgentAccount
	bindings map[providerAccountBinding]string
}

func (s *providerAccountStore) ListAgentAccounts(context.Context) ([]domain.AgentAccount, error) {
	return s.accounts, nil
}
func (s *providerAccountStore) GetAgentAccountDefault(_ context.Context, provider domain.AgentAccountProvider) (string, bool, error) {
	for _, account := range s.accounts {
		if account.Provider == provider {
			return account.ID, true, nil
		}
	}
	return "", false, nil
}
func (s *providerAccountStore) BindSessionAgentAccount(_ context.Context, session domain.SessionID, provider domain.AgentAccountProvider, account string, _ time.Time) error {
	s.bindings[providerAccountBinding{session, provider}] = account
	return nil
}
func (s *providerAccountStore) GetSessionAgentAccount(_ context.Context, session domain.SessionID, provider domain.AgentAccountProvider) (string, bool, error) {
	account, ok := s.bindings[providerAccountBinding{session, provider}]
	return account, ok, nil
}

type providerAccountCLI struct {
	ports.AgentAccountProviderCLI
	prepareErr error
}

func (p *providerAccountCLI) Prepare(home string) error {
	if p.prepareErr != nil {
		return p.prepareErr
	}
	return p.AgentAccountProviderCLI.Prepare(home)
}
func (p *providerAccountCLI) Identity(_ context.Context, home string) (domain.AgentAccountIdentityCheck, error) {
	if home == "" {
		return domain.AgentAccountIdentityCheck{}, nil
	}
	if p.Provider() == domain.AgentAccountProviderCodex {
		if _, err := os.Lstat(filepath.Join(home, "auth.json")); errors.Is(err, os.ErrNotExist) {
			return domain.AgentAccountIdentityCheck{}, nil // normal provider logout
		}
	}
	return domain.AgentAccountIdentityCheck{SignedIn: true, LoggedIn: true}, nil
}
func (*providerAccountCLI) Limits(context.Context, string) (domain.AgentAccountLimits, error) {
	return domain.AgentAccountLimits{}, nil
}

func TestAccountValidationOnlyBlocksItsProvider(t *testing.T) {
	for _, failure := range []string{"codex-signed-out", "codex-unavailable", "claude-unsafe"} {
		for _, harness := range []domain.AgentHarness{domain.HarnessClaudeCode, domain.HarnessCodex} {
			for _, operation := range []string{"tui-spawn", "chat-spawn", "tui-restore", "chat-restore", "cleanup"} {
				t.Run(failure+"/"+string(harness)+"/"+operation, func(t *testing.T) {
					ctx := context.Background()
					root := filepath.Join(t.TempDir(), "accounts")
					catalog := &providerAccountStore{bindings: map[providerAccountBinding]string{}}
					providers := map[domain.AgentAccountProvider]*providerAccountCLI{
						domain.AgentAccountProviderClaude: {AgentAccountProviderCLI: &agentaccount.Claude{}},
						domain.AgentAccountProviderCodex:  {AgentAccountProviderCLI: &agentaccount.Codex{}},
					}
					for _, p := range domain.AgentAccountProviders {
						home := filepath.Join(root, string(p), "saved")
						if err := providers[p].Prepare(home); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte("fake-credential"), 0o600); err != nil {
							t.Fatal(err)
						}
						catalog.accounts = append(catalog.accounts, domain.AgentAccount{ID: string(p), Provider: p, Home: home, ProfileKind: domain.AgentAccountProfileContained})
						catalog.bindings[providerAccountBinding{"session-1", p}] = string(p)
					}
					accounts := agentaccounts.New(catalog, []ports.AgentAccountProviderCLI{providers[domain.AgentAccountProviderClaude], providers[domain.AgentAccountProviderCodex]}, root, nil, nil)
					// Cache the valid binding first; launch must still detect logout.
					if _, err := accounts.List(ctx, true); err != nil {
						t.Fatal(err)
					}
					broken, wantErr := domain.AgentAccountProviderCodex, ports.ErrAgentAccountSignedOut
					switch failure {
					case "codex-signed-out":
						if err := os.Remove(filepath.Join(root, "codex", "saved", "auth.json")); err != nil {
							t.Fatal(err)
						}
					case "codex-unavailable":
						wantErr = ports.ErrAgentAccountStorageUnavailable
						providers[broken].prepareErr = wantErr
					case "claude-unsafe":
						broken, wantErr = domain.AgentAccountProviderClaude, ports.ErrAgentAccountStorageUnsafe
						if err := os.Symlink(t.TempDir(), filepath.Join(root, "claude", "saved", "unknown-link")); err != nil {
							t.Fatal(err)
						}
					}
					mode := domain.SessionModeTUI
					if operation == "chat-spawn" || operation == "chat-restore" {
						mode = domain.SessionModeChat
					}
					manager, store, runtime, chat, _ := newTransitionManager(t, mode)
					manager.agents = singleAgent{agent: &recordingAgent{}}
					manager.dataDir = t.TempDir()
					manager.SetAgentAccounts(accounts)
					rec := store.sessions["session-1"]
					rec.Harness, rec.IsTerminated = harness, true
					store.sessions[rec.ID] = rec
					var err error
					var env map[string]string
					cleanup := &cleaningAgent{}
					switch operation {
					case "tui-spawn", "chat-spawn":
						p, _ := domain.AgentAccountProviderFor(harness)
						_, _, _, err = manager.Spawn(ctx, ports.SpawnConfig{ProjectID: "proj", Kind: domain.KindWorker, Harness: harness, RequestedMode: mode, AgentAccountID: string(p)})
					case "tui-restore", "chat-restore":
						_, err = manager.RestoreWithMode(ctx, rec.ID)
					case "cleanup":
						manager.agents = singleAgent{agent: cleanup}
						manager.cleanupAgentWorkspace(ctx, rec, rec.Metadata.WorkspacePath)
					}
					active, _ := domain.AgentAccountProviderFor(harness)
					if active == broken {
						if operation != "cleanup" && !errors.Is(err, wantErr) {
							t.Fatalf("active provider error: %v, want %v", err, wantErr)
						}
						if runtime.created != 0 || chat.start.SessionID != "" || cleanup.cleanupCalls != 0 {
							t.Fatal("active invalid account started or cleaned a controller")
						}
						return
					}
					if err != nil {
						t.Fatalf("other provider blocked launch: %v", err)
					}
					if operation == "cleanup" {
						if cleanup.cleanupCalls != 1 {
							t.Fatal("other provider blocked cleanup")
						}
						env = cleanup.cleanupConfigs[0].Env
					} else if mode == domain.SessionModeChat {
						env = chat.start.Env
					} else {
						if runtime.created != 1 {
							t.Fatal("valid active provider did not start")
						}
						env = runtime.lastCfg.Env
					}
					if env["CODEX_HOME"] != filepath.Join(root, "codex", "saved") || env["CLAUDE_CONFIG_DIR"] != filepath.Join(root, "claude", "saved") || env["AO_CODEX_MANAGED_ACCOUNT"] != "1" {
						t.Fatal("other provider lost its pinned managed environment")
					}
				})
			}
		}
	}
}
