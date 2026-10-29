//go:build !linux

package agentaccount

import "github.com/aoagents/agent-orchestrator/backend/internal/ports"

func prepareContained(home, _ string, _ []string) error {
	if home == "" {
		return nil
	}
	return ports.ErrAgentAccountStorageUnsupported
}
