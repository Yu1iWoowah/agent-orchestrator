//go:build !linux

package hcg

import (
	"context"

	"github.com/aoagents/agent-orchestrator/components/hcg/reporting"
)

func readCredential(context.Context, string, reporting.DelegationCredentialReference) ([]byte, error) {
	return nil, errProviderTransport
}
