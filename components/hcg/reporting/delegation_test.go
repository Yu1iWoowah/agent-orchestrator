package reporting

import (
	"context"
	"errors"
	"testing"

	"github.com/aoagents/agent-orchestrator/components/hcg/internal/delegate"
)

type providerFunc func(context.Context, DelegationProviderRequest) (DelegationProviderResponse, error)

func (f providerFunc) Call(ctx context.Context, r DelegationProviderRequest) (DelegationProviderResponse, error) {
	return f(ctx, r)
}

// The fixed seam hands the parent's private capture to the attempt owner
// unchanged and forwards only the checked Body.
func TestFixedDelegationProviderPassesPrivateCapture(t *testing.T) {
	capture := &DelegationProviderCapture{Outcome: "response", Body: []byte("raw\r\n"), Complete: true, ObservedAtLeast: 5, Stage: "credential_scan", Code: "credential_echo"}
	refused := errors.New("synthetic refusal")
	p := fixedDelegationProvider{provider: providerFunc(func(context.Context, DelegationProviderRequest) (DelegationProviderResponse, error) {
		return DelegationProviderResponse{Status: 200, RequestID: "req", Capture: capture}, refused
	})}
	got, err := p.Do(t.Context(), delegate.WireRequest{Provider: "openai"})
	if !errors.Is(err, refused) || got.Capture != capture || len(got.Body) != 0 || got.Headers["x-request-id"] != "req" {
		t.Fatalf("capture changed or raw forwarded: %+v %v", got, err)
	}
}
