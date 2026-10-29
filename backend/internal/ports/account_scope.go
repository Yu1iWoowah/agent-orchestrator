package ports

import (
	"context"
	"net/url"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

type agentAccountContextKey struct{}

// WithAgentAccount carries a daemon-resolved selection through preflight. It
// contains no home/env and cannot be populated from an untrusted request body.
func WithAgentAccount(ctx context.Context, selected domain.ResolvedAgentAccount) context.Context {
	return context.WithValue(ctx, agentAccountContextKey{}, selected)
}

// AgentAccountFromContext reads a trusted daemon resolution.
func AgentAccountFromContext(ctx context.Context) (domain.ResolvedAgentAccount, bool) {
	selected, ok := ctx.Value(agentAccountContextKey{}).(domain.ResolvedAgentAccount)
	return selected, ok && selected.AccountID != ""
}

const accountModelScopePrefix = "@account:"

// AccountModelCatalogScope separates native profile catalogs. Discovery uses
// an AO-owned neutral working directory, so projects with the same account share
// its catalog; project env cannot change its credential context.
func AccountModelCatalogScope(accountID string) string {
	if accountID == "" || accountID == domain.DeviceAgentAccountID(domain.AgentAccountProviderClaude) || accountID == domain.DeviceAgentAccountID(domain.AgentAccountProviderCodex) {
		return ""
	}
	return accountModelScopePrefix + url.QueryEscape(accountID)
}

// ParseAccountModelCatalogScope decodes an internal managed catalog key.
func ParseAccountModelCatalogScope(scope string) (accountID string, ok bool) {
	rest, hasPrefix := strings.CutPrefix(scope, accountModelScopePrefix)
	if !hasPrefix {
		return "", false
	}
	id, err := url.QueryUnescape(rest)
	if err != nil || id == "" {
		return "", false
	}
	return id, true
}

// AgentAccountExecution provides exact-account native probes and environments.
type AgentAccountExecution interface {
	AgentAccountResolver
	AccountExecutionEnv(context.Context, domain.AgentHarness, string) (map[string]string, error)
	AccountIdentity(context.Context, domain.AgentHarness, string) (domain.AgentAccountIdentityCheck, error)
}

// AgentAccountReadinessProvider checks installation plus the selected profile,
// without querying the device/default login for a managed execution.
type AgentAccountReadinessProvider interface {
	EnsureAgentAccountReadiness(context.Context, string, domain.AgentReadinessPurpose, string) (domain.AgentReadinessSnapshot, error)
}

type remoteAccountRequestKey struct{}

// WithRemoteAccountRequest is set only by a trusted transport boundary, never
// by Host/Origin or another caller-controlled header.
func WithRemoteAccountRequest(ctx context.Context) context.Context {
	return context.WithValue(ctx, remoteAccountRequestKey{}, true)
}

// IsRemoteAccountRequest reads listener provenance, never client headers.
func IsRemoteAccountRequest(ctx context.Context) bool {
	remote, _ := ctx.Value(remoteAccountRequestKey{}).(bool)
	return remote
}

// AgentAccountProjects supplies daemon-owned project snapshots to preflight.
type AgentAccountProjects interface {
	GetProject(context.Context, string) (domain.ProjectRecord, bool, error)
}
