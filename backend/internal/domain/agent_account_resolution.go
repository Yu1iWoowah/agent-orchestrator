package domain

import (
	"fmt"
	"maps"
	"strings"
)

// AgentAccountRole selects the local role reference, independent of model/effort.
type AgentAccountRole string

// Supported local execution roles.
const (
	AgentAccountRoleWorker       AgentAccountRole = "worker"
	AgentAccountRoleOrchestrator AgentAccountRole = "orchestrator"
	AgentAccountRoleReviewer     AgentAccountRole = "reviewer"
)

// AgentAccountResolutionSource records why a new execution chose its account.
// It is a durable fact; changing defaults never rewrites an existing binding.
type AgentAccountResolutionSource string

// Durable resolution sources, including migrated legacy choices.
const (
	AgentAccountSourceExplicit AgentAccountResolutionSource = "explicit"
	AgentAccountSourceRole     AgentAccountResolutionSource = "role"
	AgentAccountSourceProject  AgentAccountResolutionSource = "project"
	AgentAccountSourceGlobal   AgentAccountResolutionSource = "global"
	AgentAccountSourceDevice   AgentAccountResolutionSource = "device"
	AgentAccountSourceLegacy   AgentAccountResolutionSource = "legacy"
)

// ResolvedAgentAccount is safe to return through the API: no profile path/env.
type ResolvedAgentAccount struct {
	AccountID string                       `json:"accountId"`
	Provider  AgentAccountProvider         `json:"provider"`
	Source    AgentAccountResolutionSource `json:"source" enum:"explicit,role,project,global,device,legacy"`
}

// RoleAccountReference returns only the configured role's local reference.
// Omitted references inherit; invalid references are rejected by the resolver.
func (c ProjectConfig) RoleAccountReference(role AgentAccountRole, harness AgentHarness) string {
	switch role {
	case AgentAccountRoleWorker:
		return c.Worker.AgentAccountID
	case AgentAccountRoleOrchestrator:
		return c.Orchestrator.AgentAccountID
	case AgentAccountRoleReviewer:
		for _, reviewer := range c.Reviewers {
			if string(reviewer.Harness) == string(harness) {
				return reviewer.AgentAccountID
			}
		}
	}
	return ""
}

// ValidateAccountReferences checks shape/provider compatibility without probing
// provider credentials or trusting display strings as filesystem paths.
func (c ProjectConfig) ValidateAccountReferences() error {
	validID := func(id string) bool {
		return len(id) <= 200 && strings.TrimSpace(id) == id && !strings.ContainsAny(id, "/\\\x00\r\n")
	}
	for provider, id := range c.AgentAccounts {
		if provider != AgentAccountProviderClaude && provider != AgentAccountProviderCodex {
			return fmt.Errorf("agentAccounts: unsupported provider")
		}
		if !validID(id) {
			return fmt.Errorf("agentAccounts: invalid local account reference")
		}
	}
	for _, role := range []RoleOverride{c.Worker, c.Orchestrator} {
		if !validID(role.AgentAccountID) {
			return fmt.Errorf("role: invalid local account reference")
		}
		if role.AgentAccountID != "" && role.Harness != "" {
			if _, ok := AgentAccountProviderFor(role.Harness); !ok {
				return fmt.Errorf("role: harness does not support local accounts")
			}
		}
	}
	seenReviewers := map[ReviewerHarness]string{}
	for _, r := range c.Reviewers {
		if previous, seen := seenReviewers[r.Harness]; seen && previous != r.AgentAccountID {
			return fmt.Errorf("reviewer: conflicting local account references for one harness")
		}
		seenReviewers[r.Harness] = r.AgentAccountID
		if !validID(r.AgentAccountID) {
			return fmt.Errorf("reviewer: invalid local account reference")
		}
		if r.AgentAccountID != "" {
			if _, ok := AgentAccountProviderFor(AgentHarness(r.Harness)); !ok {
				return fmt.Errorf("reviewer: harness does not support local accounts")
			}
		}
	}
	return nil
}

// WithoutLocalAccounts is the allowlist projection for Cloud/sync/export.
// Copy the reviewer slice so projecting it cannot clear the local configuration.
func (c ProjectConfig) WithoutLocalAccounts() ProjectConfig {
	c.AgentAccounts = nil
	c.Env = maps.Clone(c.Env)
	for key := range c.Env {
		if localAccountEnvKey(key) {
			delete(c.Env, key)
		}
	}
	c.Worker.AgentAccountID = ""
	c.Orchestrator.AgentAccountID = ""
	c.Reviewers = append([]ReviewerConfig(nil), c.Reviewers...)
	for i := range c.Reviewers {
		c.Reviewers[i].AgentAccountID = ""
	}
	return c
}

// HasLocalAccountReferences reports opaque references private to this host.
func (c ProjectConfig) HasLocalAccountReferences() bool {
	for _, id := range c.AgentAccounts {
		if id != "" {
			return true
		}
	}
	if c.Worker.AgentAccountID != "" || c.Orchestrator.AgentAccountID != "" {
		return true
	}
	for _, r := range c.Reviewers {
		if r.AgentAccountID != "" {
			return true
		}
	}
	return false
}

// localAccountEnvKey mirrors the native managed-account adapters' credential
// and routing overrides, and their home selectors. These belong to this host,
// never a Cloud/export payload. Ordinary project env keeps its existing meaning.
func localAccountEnvKey(key string) bool {
	switch strings.ToUpper(key) {
	case "CODEX_HOME", "AO_CODEX_MANAGED_ACCOUNT", "CLAUDE_CONFIG_DIR", "CLAUDE_SECURESTORAGE_CONFIG_DIR",
		"OPENAI_API_KEY", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN",
		"ANTHROPIC_BASE_URL", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY":
		return true
	}
	return false
}

// HasLocalAccountSettings also detects host-private credential environment.
func (c ProjectConfig) HasLocalAccountSettings() bool {
	if c.HasLocalAccountReferences() {
		return true
	}
	for key, value := range c.Env {
		if value != "" && localAccountEnvKey(key) {
			return true
		}
	}
	return false
}

// WithLocalAccountReferences restores host-private settings omitted from a
// remote projection. Reviewer identity is its harness, never an array index.
func (c ProjectConfig) WithLocalAccountReferences(previous ProjectConfig) ProjectConfig {
	c.AgentAccounts = maps.Clone(previous.AgentAccounts)
	c.Worker.AgentAccountID = previous.Worker.AgentAccountID
	c.Orchestrator.AgentAccountID = previous.Orchestrator.AgentAccountID
	c.Reviewers = append([]ReviewerConfig(nil), c.Reviewers...)
	byHarness := make(map[ReviewerHarness]string, len(previous.Reviewers))
	for _, r := range previous.Reviewers {
		byHarness[r.Harness] = r.AgentAccountID
	}
	for i := range c.Reviewers {
		c.Reviewers[i].AgentAccountID = byHarness[c.Reviewers[i].Harness]
	}
	c.Env = maps.Clone(c.Env)
	for key, value := range previous.Env {
		if localAccountEnvKey(key) {
			if c.Env == nil {
				c.Env = map[string]string{}
			}
			c.Env[key] = value
		}
	}
	return c
}

// AgentAccountBindingView is a non-secret read projection. Available reflects
// metadata and known observations; every launch still performs native preflight.
type AgentAccountBindingView struct {
	AccountID string                       `json:"accountId"`
	Provider  AgentAccountProvider         `json:"provider"`
	Label     string                       `json:"label"`
	Source    AgentAccountResolutionSource `json:"source" enum:"explicit,role,project,global,device,legacy"`
	Available bool                         `json:"available"`
}
