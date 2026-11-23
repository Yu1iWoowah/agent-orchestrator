// Package integration holds the AO-owned session identity and policy
// resolution shared by the fixed delegation consumer.
package integration

import "github.com/aoagents/agent-orchestrator/components/hcg/policy"

type PolicyInputs = policy.Inputs
type Policy = policy.Policy

func ResolvePolicy(role string, in PolicyInputs) Policy {
	return policy.Resolve(role, in)
}

// Identity comes from AO-owned session facts, never from the tool or HTTP body.
type Identity struct {
	DataRoot             string `json:"dataRoot"`
	ProjectID            string `json:"projectId"`
	SessionID            string `json:"sessionId"`
	Role                 string `json:"role"`
	Mode                 string `json:"mode"`
	Harness              string `json:"harness"`
	Worktree             string `json:"worktree"`
	ArtifactDir          string `json:"artifactDir"`
	NativeID             string `json:"nativeId"`
	LaunchID             string `json:"launchId"`
	ControllerGeneration string `json:"controllerGeneration"`
}
