package sessionmanager

import (
	"context"
	"errors"
	"io"
	"io/fs"
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

// Only the account's own history link may lead out of its home. A link deeper
// down that points outside AO's history must not receive the copy, and the
// launch still stays on the account.
func TestImportCopyRefusesALinkOutOfAOHistory(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	source := filepath.Join(root, "native-home")
	transcript := filepath.Join(source, "projects", "-work", "native.jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcript, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	aoHistory := filepath.Join(root, "ao", "history", "claude")
	account := filepath.Join(root, "ao", "accounts", "acct-1")
	outside := filepath.Join(root, "outside")
	for _, dir := range []string{aoHistory, account, outside} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(aoHistory, filepath.Join(account, "projects")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(aoHistory, "-work")); err != nil {
		t.Fatal(err)
	}
	accounts := newTransitionAccounts(&[]string{})
	m := &Manager{agents: importHomeAgents{}, agentAccounts: accounts, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	rec := domain.SessionRecord{ID: "imported", Harness: domain.HarnessClaudeCode}
	rec.Metadata = domain.SessionMetadata{ProviderConversationID: "native", NativeTranscriptPath: transcript,
		ImportSource: &domain.SessionImportSource{NativeID: "native", ConfigDir: source}}
	accounts.bound[rec.ID] = "acct-1"
	env := map[string]string{"CLAUDE_CONFIG_DIR": account}
	m.applyImportedConfigEnv(context.Background(), rec, env)
	if env["CLAUDE_CONFIG_DIR"] != account {
		t.Fatalf("launch left the account: %v", env)
	}
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Fatalf("copy escaped AO history into %s: %v %v", outside, entries, err)
	}
}

// pausingReader reports, partway through the copy, whether the final name
// already exists; an interrupted daemon would leave whatever stands there.
type pausingReader struct {
	data    []byte
	read    int
	midway  func()
	failAt  int
	stopped bool
}

func (r *pausingReader) Read(p []byte) (int, error) {
	if r.read >= len(r.data) {
		return 0, io.EOF
	}
	if r.read > 0 && r.midway != nil {
		r.midway()
		r.midway = nil
	}
	if r.failAt > 0 && r.read >= r.failAt {
		r.stopped = true
		return 0, io.ErrUnexpectedEOF
	}
	n := copy(p[:min(len(p), 4)], r.data[r.read:])
	r.read += n
	return n, nil
}

// The conversation appears under its final name only once complete. A copy cut
// short leaves nothing there, so the next launch copies it again instead of
// taking a truncated file as the conversation.
func TestImportCopyPublishesOnlyAFinishedConversation(t *testing.T) {
	root := t.TempDir()
	account := filepath.Join(root, "acct")
	aoHistory := filepath.Join(root, "history")
	for _, dir := range []string{account, aoHistory} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(aoHistory, filepath.Join(account, "projects")); err != nil {
		t.Fatal(err)
	}
	name := "projects/-work/native.jsonl"
	final := filepath.Join(account, filepath.FromSlash(name))
	content := []byte("{\"turn\":1}\n{\"turn\":2}\n")

	visibleMidway := true
	cut := &pausingReader{data: content, failAt: 8, midway: func() {
		_, err := os.Stat(final)
		visibleMidway = err == nil
	}}
	if err := publishImportedTranscript(cut, account, name); err == nil || !cut.stopped {
		t.Fatalf("interrupted copy reported %v", err)
	}
	if visibleMidway {
		t.Fatal("the unfinished conversation stood at its final name during the copy")
	}
	if _, err := os.Stat(final); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("an interrupted copy left the final name: %v", err)
	}

	if err := publishImportedTranscript(&pausingReader{data: content}, account, name); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(final); err != nil || string(got) != string(content) {
		t.Fatalf("next launch did not place the whole conversation: %q %v", got, err)
	}
	entries, err := os.ReadDir(filepath.Dir(final))
	if err != nil || len(entries) != 1 {
		t.Fatalf("staging files left behind: %v %v", entries, err)
	}
	if err := publishImportedTranscript(&pausingReader{data: []byte("other")}, account, name); err == nil {
		t.Fatal("an existing conversation was replaced")
	}
	if got, _ := os.ReadFile(final); string(got) != string(content) {
		t.Fatalf("existing conversation changed: %q", got)
	}
}
