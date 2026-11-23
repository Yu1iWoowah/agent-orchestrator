package delegate

import "github.com/aoagents/agent-orchestrator/components/hcg/policy"

type PolicyInputs = policy.Inputs
type Policy = policy.Policy

func ResolvePolicy(role string, in PolicyInputs) Policy {
	return policy.Resolve(role, in)
}

// Identity comes from AO-owned session facts, never from the tool or HTTP body.
// ReadRoots are extra read-only roots beyond the worktree (orchestrator only).
type Identity struct {
	DataRoot  string   `json:"dataRoot"`
	ProjectID string   `json:"projectId"`
	SessionID string   `json:"sessionId"`
	Role      string   `json:"role"`
	Mode      string   `json:"mode"`
	Harness   string   `json:"harness"`
	Worktree  string   `json:"worktree"`
	ReadRoots []string `json:"readRoots"`
}
