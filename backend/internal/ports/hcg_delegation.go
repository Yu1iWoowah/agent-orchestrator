package ports

import "github.com/aoagents/agent-orchestrator/backend/internal/domain"

// HCGDelegationMount is pre-launch typed metadata, not a grant/controller owner.
type HCGDelegationMount struct {
	SessionID domain.SessionID
	ProjectID domain.ProjectID
	Kind      domain.SessionKind
	Harness   domain.AgentHarness
	Mode      domain.SessionMode
	DataRoot  string
}

// HCGDelegationMountResolver resolves only fixed pre-launch metadata.
type HCGDelegationMountResolver interface{ DelegationMountRequested(HCGDelegationMount) bool }

// EnvHCGDelegationMCP is set only by the native launch coordinator.
const EnvHCGDelegationMCP = "AO_HCG_DELEGATION_MCP"
