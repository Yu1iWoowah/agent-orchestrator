package sessionmanager

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/nativeconfig"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type codexHomeAgent struct{ fakeAgent }

func (codexHomeAgent) NativeSessionConfigDir(_ context.Context, env map[string]string) (string, error) {
	return nativeconfig.Resolve(env, "CODEX_HOME", ".codex")
}

type importHomeAgents struct{}

func (importHomeAgents) Agent(h domain.AgentHarness) (ports.Agent, bool) {
	if h == domain.HarnessCodex {
		return codexHomeAgent{}, true
	}
	return providerHomeAgent{}, true
}

// A fork-managed account profile links only AO's own history, so a conversation
// imported from the computer's provider home is not there. The launch must stay
// on the account the session is bound to and find the conversation in AO's
// history, never fall back to the computer's home and login (UPSYNC-PR2-B I1,
// user decision A-Q1=A).
func TestImportOnManagedAccountCopiesConversationInsteadOfLeavingTheAccount(t *testing.T) {
	for _, tc := range []struct {
		harness domain.AgentHarness
		key     string
		history string
		rel     string
	}{
		{domain.HarnessClaudeCode, "CLAUDE_CONFIG_DIR", "projects", filepath.Join("projects", "-work", "native.jsonl")},
		{domain.HarnessCodex, "CODEX_HOME", "sessions", filepath.Join("sessions", "2026", "11", "17", "rollout-native.jsonl")},
	} {
		t.Run(string(tc.harness), func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("HOME", root)
			source := filepath.Join(root, "native-home")
			transcript := filepath.Join(source, tc.rel)
			if err := os.MkdirAll(filepath.Dir(transcript), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(transcript, []byte("{\"turn\":1}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(source, "credentials.json"), []byte("secret"), 0o600); err != nil {
				t.Fatal(err)
			}
			// The contained layout: the account's history folder links AO's own history.
			aoHistory := filepath.Join(root, "ao", "history", string(tc.harness))
			account := filepath.Join(root, "ao", "accounts", "acct-1")
			if err := os.MkdirAll(aoHistory, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(account, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(aoHistory, filepath.Join(account, tc.history)); err != nil {
				t.Fatal(err)
			}

			accounts := newTransitionAccounts(&[]string{})
			m := &Manager{agents: importHomeAgents{}, agentAccounts: accounts, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
			rec := domain.SessionRecord{ID: "imported", Harness: tc.harness}
			rec.Metadata = domain.SessionMetadata{ProviderConversationID: "native", NativeTranscriptPath: transcript,
				ImportSource: &domain.SessionImportSource{NativeID: "native", ConfigDir: source}}

			accounts.bound[rec.ID] = "acct-1"
			env := map[string]string{tc.key: account}
			m.applyImportedConfigEnv(context.Background(), rec, env)
			if env[tc.key] != account {
				t.Fatalf("%s left the bound account for %q", tc.key, env[tc.key])
			}
			copied, err := os.ReadFile(filepath.Join(aoHistory, strings.SplitN(filepath.ToSlash(tc.rel), "/", 2)[1]))
			if err != nil || string(copied) != "{\"turn\":1}\n" {
				t.Fatalf("conversation not in AO history: %q %v", copied, err)
			}
			if _, err := os.Stat(filepath.Join(account, "credentials.json")); err == nil {
				t.Fatal("more than the conversation was copied")
			}
			// A second launch finds the copy and leaves it alone.
			m.applyImportedConfigEnv(context.Background(), rec, env)
			if env[tc.key] != account {
				t.Fatalf("second launch left the account: %v", env)
			}

			// On the computer's own login the source home is the account's home.
			accounts.bound[rec.ID] = domain.DeviceAgentAccountID(mustAccountProvider(t, tc.harness))
			env = map[string]string{tc.key: filepath.Join(root, "elsewhere")}
			m.applyImportedConfigEnv(context.Background(), rec, env)
			if env[tc.key] != source {
				t.Fatalf("device login not pointed at the source home: %v", env)
			}
		})
	}
}

func mustAccountProvider(t *testing.T, h domain.AgentHarness) domain.AgentAccountProvider {
	t.Helper()
	p, ok := domain.AgentAccountProviderFor(h)
	if !ok {
		t.Fatalf("%s has no accounts", h)
	}
	return p
}
