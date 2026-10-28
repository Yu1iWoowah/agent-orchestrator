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

type launchBoundaryStore struct {
	ports.AgentAccountStore
	account domain.AgentAccount
	bound   map[domain.SessionID]string
}

func (s *launchBoundaryStore) ListAgentAccounts(context.Context) ([]domain.AgentAccount, error) {
	return []domain.AgentAccount{s.account}, nil
}
func (s *launchBoundaryStore) GetAgentAccountDefault(context.Context, domain.AgentAccountProvider) (string, bool, error) {
	return s.account.ID, true, nil
}
func (s *launchBoundaryStore) BindSessionAgentAccount(_ context.Context, id domain.SessionID, _ domain.AgentAccountProvider, account string, _ time.Time) error {
	s.bound[id] = account
	return nil
}
func (s *launchBoundaryStore) GetSessionAgentAccount(_ context.Context, id domain.SessionID, _ domain.AgentAccountProvider) (string, bool, error) {
	account, ok := s.bound[id]
	return account, ok, nil
}

type launchBoundaryProvider struct{ ports.AgentAccountProviderCLI }

func (launchBoundaryProvider) Identity(_ context.Context, home string) (domain.AgentAccountIdentityCheck, error) {
	return domain.AgentAccountIdentityCheck{SignedIn: home != "", LoggedIn: home != ""}, nil
}
func (launchBoundaryProvider) Limits(context.Context, string) (domain.AgentAccountLimits, error) {
	return domain.AgentAccountLimits{}, nil
}

type expireAccountAfterBind struct {
	*agentaccounts.Service
	expire func()
}

func (a *expireAccountAfterBind) BindSession(ctx context.Context, id domain.SessionID, harness domain.AgentHarness, account string) error {
	if err := a.Service.BindSession(ctx, id, harness, account); err != nil {
		return err
	}
	a.expire()
	return nil
}

func TestAccountFailureRefusesSpawnAndRestore(t *testing.T) {
	for _, provider := range domain.AgentAccountProviders {
		for _, mode := range []domain.SessionMode{domain.SessionModeTUI, domain.SessionModeChat} {
			for _, operation := range []string{"spawn", "restore"} {
				t.Run(string(provider)+"/"+string(mode)+"/"+operation, func(t *testing.T) {
					ctx := context.Background()
					root := filepath.Join(t.TempDir(), "accounts")
					home := filepath.Join(root, string(provider), "saved")
					harness, credential := domain.HarnessClaudeCode, ".credentials.json"
					var adapter ports.AgentAccountProviderCLI = &agentaccount.Claude{}
					wantErr := ports.ErrAgentAccountStorageUnsafe
					if provider == domain.AgentAccountProviderCodex {
						harness, credential = domain.HarnessCodex, "auth.json"
						adapter, wantErr = &agentaccount.Codex{}, ports.ErrAgentAccountCredentialStoreUnsupported
					}
					if err := adapter.Prepare(home); err != nil {
						t.Fatal(err)
					}
					path := filepath.Join(home, credential)
					if err := os.WriteFile(path, []byte("fake-credential"), 0o600); err != nil {
						t.Fatal(err)
					}
					catalog := &launchBoundaryStore{account: domain.AgentAccount{ID: "saved", Provider: provider, Home: home, ProfileKind: domain.AgentAccountProfileContained}, bound: map[domain.SessionID]string{"session-1": "saved"}}
					accounts := agentaccounts.New(catalog, []ports.AgentAccountProviderCLI{launchBoundaryProvider{adapter}}, root, nil, nil)
					expire := func() {
						if err := os.Remove(path); err != nil {
							t.Fatal(err)
						}
						if provider == domain.AgentAccountProviderClaude {
							if err := os.Symlink(filepath.Join(t.TempDir(), "fake-device-credential"), path); err != nil {
								t.Fatal(err)
							}
						}
					}
					manager, store, runtime, chat, _ := newTransitionManager(t, mode)
					manager.agents = singleAgent{agent: &recordingAgent{}}
					manager.dataDir = t.TempDir()
					manager.SetAgentAccounts(accounts)
					var err error
					if operation == "spawn" {
						manager.SetAgentAccounts(&expireAccountAfterBind{accounts, expire})
						_, _, _, err = manager.Spawn(ctx, ports.SpawnConfig{ProjectID: "proj", Kind: domain.KindWorker, Harness: harness, RequestedMode: mode, AgentAccountID: "saved"})
					} else {
						expire()
						rec := store.sessions["session-1"]
						rec.Harness, rec.IsTerminated = harness, true
						store.sessions[rec.ID] = rec
						_, err = manager.RestoreWithMode(ctx, rec.ID)
					}
					if !errors.Is(err, wantErr) {
						t.Fatalf("%s did not refuse account failure: %v", operation, err)
					}
					if runtime.created != 0 || chat.start.SessionID != "" {
						t.Fatalf("account failure started a controller: runtime=%d chat=%q", runtime.created, chat.start.SessionID)
					}
				})
			}
		}
	}
}
