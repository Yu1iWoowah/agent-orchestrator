package agentaccounts

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestResolveAccountPrecedenceAndInvalidReferences(t *testing.T) {
	for _, tc := range []struct {
		name, explicit, role, project, global, want, source string
		err                                                 error
	}{
		{"explicit wins", "explicit", "role", "project", "global", "explicit", "explicit", nil},
		{"role wins", "", "role", "project", "global", "role", "role", nil},
		{"project wins", "", "", "project", "global", "project", "project", nil},
		{"global wins", "", "", "", "global", "global", "global", nil},
		{"legacy device only when unset", "", "", "", "", "device-claude", "device", nil},
		{"explicit absent", "gone", "role", "project", "global", "", "", ports.ErrAgentAccountUnknown},
		{"role absent", "", "gone", "project", "global", "", "", ports.ErrAgentAccountUnknown},
		{"project absent", "", "", "gone", "global", "", "", ports.ErrAgentAccountUnknown},
		{"global absent", "", "", "", "gone", "", "", ports.ErrAgentAccountUnknown},
		{"explicit wrong provider", "other", "role", "project", "global", "", "", ports.ErrAgentAccountProviderMismatch},
		{"role retired", "", "retired", "project", "global", "", "", ports.ErrAgentAccountUnknown},
		{"global signed out no fallback", "", "", "", "signed-out", "", "", ports.ErrAgentAccountSignedOut},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "accounts")
			st := &memStore{defaults: map[domain.AgentAccountProvider]string{}, bindings: map[domain.SessionID]string{}}
			cli := &fakeClaude{signedInAs: map[string]string{"": "device@example.test"}}
			for _, id := range []string{"explicit", "role", "project", "global", "retired", "signed-out", "other"} {
				home := filepath.Join(root, "claude", id)
				if err := os.MkdirAll(home, 0700); err != nil {
					t.Fatal(err)
				}
				a := domain.AgentAccount{ID: id, Home: home, Provider: domain.AgentAccountProviderClaude, Email: id + "@example.test"}
				if id == "other" {
					a.Provider = domain.AgentAccountProviderCodex
				}
				if id == "retired" {
					at := time.Unix(1, 0)
					a.RetiredAt = &at
				}
				st.accounts = append(st.accounts, a)
				if id != "signed-out" {
					cli.signedInAs[home] = a.Email
				}
			}
			if tc.global != "" {
				st.defaults[domain.AgentAccountProviderClaude] = tc.global
			}
			svc := New(st, []ports.AgentAccountProviderCLI{cli, &evidenceCodex{fakeClaude: &fakeClaude{}}}, root, nil, nil)
			cfg := domain.ProjectConfig{AgentAccounts: map[domain.AgentAccountProvider]string{domain.AgentAccountProviderClaude: tc.project}, Worker: domain.RoleOverride{AgentAccountID: tc.role}}
			svc.read(context.Background(), st.accounts[5]) // previously observed signed-out account
			got, err := svc.ResolveAccount(context.Background(), ports.AgentAccountResolutionRequest{Harness: domain.HarnessClaudeCode, Role: domain.AgentAccountRoleWorker, ProjectConfig: cfg, AccountID: tc.explicit})
			if !errors.Is(err, tc.err) {
				t.Fatalf("error=%v want %v", err, tc.err)
			}
			if tc.err == nil && (got.AccountID != tc.want || string(got.Source) != tc.source) {
				t.Fatalf("resolution=%+v want %s/%s", got, tc.want, tc.source)
			}
			if len(st.bindings) != 0 {
				t.Fatal("resolution changed a session binding")
			}
		})
	}
}

func TestRemoteRequestsCannotSelectLocalPrivateAccounts(t *testing.T) {
	cli := &fakeClaude{onIdentity: func(string) { t.Fatal("remote request started a credential probe") }}
	st := &memStore{defaults: map[domain.AgentAccountProvider]string{domain.AgentAccountProviderClaude: "private-A"}, accounts: []domain.AgentAccount{{ID: "private-A", Provider: domain.AgentAccountProviderClaude, Home: "/fake/ao/claude/private-A"}}}
	svc := New(st, []ports.AgentAccountProviderCLI{cli}, filepath.Join(t.TempDir(), "accounts"), nil, nil)
	ctx := ports.WithRemoteAccountRequest(context.Background())
	for _, id := range []string{"private-A", "unknown-id", "device-claude", ""} {
		_, err := svc.ResolveAccount(ctx, ports.AgentAccountResolutionRequest{Harness: domain.HarnessClaudeCode, AccountID: id})
		if !errors.Is(err, ports.ErrAgentAccountLocalOnly) {
			t.Fatalf("remote account %q: %v", id, err)
		}
	}
	delete(st.defaults, domain.AgentAccountProviderClaude)
	got, err := svc.ResolveAccount(ctx, ports.AgentAccountResolutionRequest{Harness: domain.HarnessClaudeCode})
	if err != nil || got.AccountID != "device-claude" {
		t.Fatalf("unconfigured legacy device compatibility: %+v %v", got, err)
	}
}

func TestResolvedDeviceBindingDoesNotProbeIdentity(t *testing.T) {
	cli := &fakeClaude{onIdentity: func(string) { t.Error("device binding probed native identity") }}
	st := &memStore{bindings: map[domain.SessionID]string{}}
	svc := New(st, []ports.AgentAccountProviderCLI{cli}, filepath.Join(t.TempDir(), "accounts"), nil, nil)
	selected, err := svc.ResolveAccount(context.Background(), ports.AgentAccountResolutionRequest{Harness: domain.HarnessClaudeCode})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.BindResolvedSession(context.Background(), "device-worker", domain.HarnessClaudeCode, selected); err != nil {
		t.Fatal(err)
	}
	if st.bindings["device-worker"] != "device-claude" {
		t.Fatal("device binding not persisted")
	}
}

func TestSessionAccountBindingNeverResolvesNewDefaults(t *testing.T) {
	st := &memStore{bindings: map[domain.SessionID]string{"worker": "original-A"}, defaults: map[domain.AgentAccountProvider]string{domain.AgentAccountProviderClaude: "new-default-D"}}
	svc := New(st, []ports.AgentAccountProviderCLI{&fakeClaude{}}, filepath.Join(t.TempDir(), "accounts"), nil, nil)
	got, err := svc.SessionAccountBinding(context.Background(), "worker", domain.HarnessClaudeCode)
	if err != nil || got.AccountID != "original-A" {
		t.Fatalf("binding adopted new default: %+v %v", got, err)
	}
	if _, err := svc.SessionAccountBinding(context.Background(), "missing", domain.HarnessClaudeCode); !errors.Is(err, ports.ErrAgentAccountUnknown) {
		t.Fatalf("missing binding fell back: %v", err)
	}
	if st.bindings["missing"] != "" {
		t.Fatal("reading a missing binding saved today's default")
	}
}
