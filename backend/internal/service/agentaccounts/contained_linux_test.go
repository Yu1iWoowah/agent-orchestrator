package agentaccounts

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agentaccount"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestNewProfilesUseContainedPreparation(t *testing.T) {
	for _, p := range domain.AgentAccountProviders {
		t.Run(string(p), func(t *testing.T) {
			root, device := filepath.Join(t.TempDir(), "accounts"), t.TempDir()
			settings := filepath.Join(device, "settings.json")
			if err := os.WriteFile(settings, []byte(`{"env":{"SECRET":"fake-device-marker"}}`), 0o600); err != nil {
				t.Fatal(err)
			}
			binary := func(context.Context) (string, error) { return "fake-provider", nil }
			var provider ports.AgentAccountProviderCLI = &agentaccount.Claude{Binary: binary, DeviceHome: device, DeviceConfig: filepath.Join(device, ".claude.json")}
			if p == domain.AgentAccountProviderCodex {
				provider = &agentaccount.Codex{Binary: binary, DeviceHome: device}
			}
			svc := New(&memStore{}, []ports.AgentAccountProviderCLI{provider}, root, fakeTerminals{}, nil)
			for range 2 {
				start, err := svc.StartLogin(context.Background(), p)
				if err != nil {
					t.Fatal(err)
				}
				home := filepath.Join(root, string(p), start.OperationID)
				got, err := os.Readlink(filepath.Join(home, "sessions"))
				if err != nil || got != filepath.Join(root, "history", string(p), "sessions") {
					t.Fatalf("new login uses external history: %q (%v)", got, err)
				}
				if _, err := os.Lstat(filepath.Join(home, "settings.json")); !os.IsNotExist(err) {
					t.Fatal("device settings imported")
				}
			}
			entries, err := os.ReadDir(device)
			if err != nil || len(entries) != 1 {
				t.Fatal("new login changed external device entries")
			}
			info, err := os.Stat(settings)
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatal("new login changed device settings mode")
			}
			data, err := os.ReadFile(settings)
			if err != nil || string(data) != `{"env":{"SECRET":"fake-device-marker"}}` {
				t.Fatal("new login changed device settings contents")
			}
		})
	}
}

type evidenceCodex struct {
	*fakeClaude
	logouts int
}

func (*evidenceCodex) Provider() domain.AgentAccountProvider { return domain.AgentAccountProviderCodex }
func (p *evidenceCodex) Logout(ctx context.Context, home string) error {
	p.logouts++
	return p.fakeClaude.Logout(ctx, home)
}

func TestVerifyManagedCodexRequiresFileEvidence(t *testing.T) {
	for _, evidence := range []string{"file", "absent", "outside", "symlink"} {
		t.Run(evidence, func(t *testing.T) {
			ctx := context.Background()
			root := filepath.Join(t.TempDir(), "accounts")
			provider := &evidenceCodex{fakeClaude: &fakeClaude{signedInAs: map[string]string{}}}
			store := &memStore{}
			svc := New(store, []ports.AgentAccountProviderCLI{provider}, root, fakeTerminals{}, nil)
			start, err := svc.StartLogin(ctx, domain.AgentAccountProviderCodex)
			if err != nil {
				t.Fatal(err)
			}
			home := filepath.Join(root, "codex", start.OperationID)
			provider.signedInAs[home] = "fake@example.invalid"
			outside := filepath.Join(t.TempDir(), "auth.json")
			if err := os.WriteFile(outside, []byte("fake-private-marker"), 0o600); err != nil {
				t.Fatal(err)
			}
			if evidence == "file" {
				err = os.WriteFile(filepath.Join(home, "auth.json"), []byte("fake-private-marker"), 0o600)
			}
			if evidence == "symlink" {
				err = os.Symlink(outside, filepath.Join(home, "auth.json"))
			}
			if err != nil {
				t.Fatal(err)
			}
			status, _, err := svc.VerifyLogin(ctx, start.OperationID)
			if evidence == "file" {
				if err != nil || status != LoginComplete || len(store.accounts) != 1 || store.accounts[0].ProfileKind != domain.AgentAccountProfileContained {
					t.Fatalf("verify=%q err=%v accounts=%d", status, err, len(store.accounts))
				}
			} else {
				if !errors.Is(err, ports.ErrAgentAccountCredentialStoreUnsupported) && !errors.Is(err, ports.ErrAgentAccountStorageUnsafe) {
					t.Fatalf("no safe refusal: %v", err)
				}
				if len(store.accounts) != 0 || provider.logouts != 0 {
					t.Fatal("unsupported login was saved or provider logout was called")
				}
				if strings.Contains(err.Error(), root) || strings.Contains(err.Error(), "fake-private-marker") {
					t.Fatal("credential evidence error leaked private information")
				}
			}
		})
	}
}

func TestMissingFileEvidenceInvalidatesManagedAccountCache(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "accounts")
	home := filepath.Join(root, "codex", "saved")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	auth := filepath.Join(home, "auth.json")
	if err := os.WriteFile(auth, []byte("fake-private-marker"), 0o600); err != nil {
		t.Fatal(err)
	}
	provider := &evidenceCodex{fakeClaude: &fakeClaude{signedInAs: map[string]string{home: "fake@example.invalid"}}}
	store := &memStore{accounts: []domain.AgentAccount{{ID: "saved", Provider: domain.AgentAccountProviderCodex, Home: home, ProfileKind: domain.AgentAccountProfileContained}}}
	svc := New(store, []ports.AgentAccountProviderCLI{provider}, root, fakeTerminals{}, nil)
	if _, err := svc.List(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(auth); err != nil {
		t.Fatal(err)
	}
	views, err := svc.List(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, view := range views {
		if view.Account.ID == "saved" && (view.SignedIn || view.IdentityErr != "account_credential_store_unsupported") {
			t.Fatalf("stale usable account: %+v", view)
		}
	}
	if err := svc.ValidateAccount(ctx, domain.HarnessCodex, "saved"); err == nil {
		t.Fatal("launch accepted missing file evidence")
	}
}

func TestSignedOutContainedCodexDefaultUsesSignedInFallback(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "accounts")
	signedOut, signedIn := filepath.Join(root, "codex", "out"), filepath.Join(root, "codex", "in")
	for _, home := range []string{signedOut, signedIn} {
		if err := os.MkdirAll(home, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(signedIn, "auth.json"), []byte("fake-credential"), 0o600); err != nil {
		t.Fatal(err)
	}
	provider := &evidenceCodex{fakeClaude: &fakeClaude{signedInAs: map[string]string{signedIn: "fallback@example.invalid"}}}
	store := &memStore{
		accounts: []domain.AgentAccount{
			{ID: "out", Provider: domain.AgentAccountProviderCodex, Home: signedOut, ProfileKind: domain.AgentAccountProfileContained},
			{ID: "in", Provider: domain.AgentAccountProviderCodex, Home: signedIn, ProfileKind: domain.AgentAccountProfileContained},
		},
		defaults: map[domain.AgentAccountProvider]string{domain.AgentAccountProviderCodex: "out"},
		bindings: map[domain.SessionID]string{},
	}
	svc := New(store, []ports.AgentAccountProviderCLI{provider}, root, fakeTerminals{}, nil)
	identity, handled := svc.DefaultSignIn(ctx, domain.HarnessCodex)
	if !handled || !identity.SignedIn || identity.Email != "fallback@example.invalid" {
		t.Fatalf("readiness did not select signed-in fallback: %+v, handled=%v", identity, handled)
	}
	if err := svc.ValidateAccount(ctx, domain.HarnessCodex, "out"); !errors.Is(err, ports.ErrAgentAccountSignedOut) {
		t.Fatalf("signed-out account misclassified: %v", err)
	}
	if err := svc.BindSession(ctx, "new", domain.HarnessCodex, ""); err != nil || store.bindings["new"] != "in" {
		t.Fatalf("binding disagrees with readiness: %q, %v", store.bindings["new"], err)
	}
}

type runtimeFixtureCodex struct {
	*evidenceCodex
	adapter agentaccount.Codex
}

func (p *runtimeFixtureCodex) Prepare(home string) error { return p.adapter.Prepare(home) }
func (p *runtimeFixtureCodex) LaunchEnv(home string) map[string]string {
	return p.adapter.LaunchEnv(home)
}

func TestCodexRuntimeFixtureRemainsUsable(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "accounts")
	home := filepath.Join(root, "codex", "saved")
	provider := &runtimeFixtureCodex{evidenceCodex: &evidenceCodex{fakeClaude: &fakeClaude{signedInAs: map[string]string{home: "fake@example.invalid"}}}}
	if err := provider.Prepare(home); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"auth.json", "models_cache.json"} {
		if err := os.WriteFile(filepath.Join(home, name), []byte("fake-provider-data"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(home, "models_cache.json"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Metadata-shaped fixture only: no actual provider binary or socket is read.
	for name, target := range map[string]string{
		"plugins/cache/openai-bundled/fake-plugin/latest":    "26.fake",
		"tmp/arg0-linux/codex-arg0-live/apply_patch":         "../../../../fake-codex-binary",
		"tmp/arg0-linux/codex-arg0-live/applypatch":          "../../../../fake-codex-binary",
		"tmp/arg0-linux/codex-arg0-live/codex-linux-sandbox": "../../../../fake-codex-binary",
		"tmp/arg0-linux/codex-arg0-stale/apply_patch":        "../../../../removed-fake-binary",
		"app-server-control/app-server-control.sock":         filepath.Join(t.TempDir(), "fake-daemon.sock"),
		"packages/standalone/current":                        "releases/fake-version",
		"packages/standalone/releases/fake-version/codex":    "../../../../fake-codex-binary",
	} {
		path := filepath.Join(home, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
	}
	store := &memStore{accounts: []domain.AgentAccount{{ID: "saved", Provider: domain.AgentAccountProviderCodex, Home: home, ProfileKind: domain.AgentAccountProfileContained}}, bindings: map[domain.SessionID]string{}}
	svc := New(store, []ports.AgentAccountProviderCLI{provider}, root, fakeTerminals{}, nil)
	if err := provider.Prepare(home); err != nil {
		t.Fatalf("runtime fixture refused: %v", err)
	}
	views, err := svc.List(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, view := range views {
		if view.Account.ID == "saved" && (!view.SignedIn || view.IdentityErr != "") {
			t.Fatalf("runtime fixture listed as unusable: %+v", view)
		}
	}
	if err := svc.BindSession(ctx, "s1", domain.HarnessCodex, "saved"); err != nil {
		t.Fatal(err)
	}
	env, err := svc.SessionLaunchEnv(ctx, "s1", domain.HarnessCodex)
	if err != nil || env["CODEX_HOME"] != home || env["AO_CODEX_MANAGED_ACCOUNT"] != "1" {
		t.Fatalf("runtime fixture cannot launch on its managed account: %v, %v", env, err)
	}
}
