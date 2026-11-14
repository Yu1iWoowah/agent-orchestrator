//go:build !linux

package hcg

import (
	"context"

	"github.com/aoagents/agent-orchestrator/components/hcg/reporting"
)

func readLabVersion(context.Context, string, reporting.DelegationCredentialReference) ([]byte, error) {
	return nil, errLabTransport
}
