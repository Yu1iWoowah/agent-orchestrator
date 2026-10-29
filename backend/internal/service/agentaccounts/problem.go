package agentaccounts

import (
	"context"
	"errors"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

var errAccountProviderUnavailable = errors.New("account_provider_unavailable")

// Provider errors can contain credential/config bytes. Keep only canonical
// categories, including in logs; arbitrary secrets cannot be safely removed by
// guessing token formats. Never retain the raw error as a public error's cause.
func safeAccountProviderError(err error) error {
	if err == nil {
		return nil
	}
	for _, category := range []error{
		ports.ErrAgentAccountCredentialStoreUnsupported,
		ports.ErrAgentAccountStorageUnsafe,
		ports.ErrAgentAccountStorageUnavailable,
		ports.ErrAgentAccountStorageUnsupported,
		ports.ErrAgentAccountUnshared,
		context.Canceled,
		context.DeadlineExceeded,
	} {
		if errors.Is(err, category) {
			return category
		}
	}
	return errAccountProviderUnavailable
}
