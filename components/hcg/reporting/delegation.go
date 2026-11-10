package reporting

import (
	"context"
	"encoding/json"

	"github.com/aoagents/agent-orchestrator/components/hcg/internal/delegate"
	"github.com/aoagents/agent-orchestrator/components/hcg/internal/integration"
)

// ErrDelegationNotSent is returned only when the trusted parent can prove no HTTP send.
var ErrDelegationNotSent = delegate.ErrNotSent

// DelegationProvider is the trusted fixed HTTP-provider seam. Without an
// explicit operator run grant, even a configured lab parent stays unavailable.
// Fixtures exercise this same caller; neither command/env nor model tool args
// can replace the endpoint, provider or immutable deployment.
type DelegationProvider interface {
	Call(context.Context, DelegationProviderRequest) (DelegationProviderResponse, error)
}
type DelegationProviderRequest struct {
	BillingProvider, ProfileID, ProfileRevision, Model, Endpoint, Workspace string
	CredentialRef                                                           DelegationCredentialReference
	Payload                                                                 []byte
}
type DelegationProviderResponse struct {
	Status                        int
	RequestID, AlternateRequestID string
	Body                          []byte
}
type DelegationConsumer struct{ core *delegate.Coordinator }
type DelegationRevision struct{ value delegate.PolicyRevision }
type PreparedDelegationLaunch struct {
	value delegate.PreparedDelegationLaunch
}

type fixedDelegationProvider struct {
	provider   DelegationProvider
	deployment delegate.Deployment
}

func (p fixedDelegationProvider) Do(ctx context.Context, r delegate.WireRequest) (delegate.WireResponse, error) {
	d := p.deployment
	response, err := p.provider.Call(ctx, DelegationProviderRequest{BillingProvider: r.Provider, ProfileID: d.Profile.ID, ProfileRevision: d.Profile.Revision, Model: r.Model, Endpoint: r.Endpoint, Workspace: d.Workspace, CredentialRef: DelegationCredentialReference(d.CredentialRef), Payload: append([]byte(nil), r.Payload...)})
	return delegate.WireResponse{Status: response.Status, Headers: map[string]string{"x-request-id": response.RequestID, "request-id": response.AlternateRequestID}, Body: response.Body}, err
}

// NewDelegation has no arbitrary executable/environment hooks. Its provider and
// checkpoint dependencies are supplied by the daemon, never by HTTP/MCP data.
func NewDelegation(root string, recorder DelegationUsageRecorder, provider DelegationProvider, checkpoint func(context.Context, string)) *DelegationConsumer {
	var factory func(delegate.Deployment) delegate.Transport
	if provider != nil {
		factory = func(d delegate.Deployment) delegate.Transport { return fixedDelegationProvider{provider, d} }
	}
	return &DelegationConsumer{core: delegate.NewCoordinator(root, delegationReceiptAdapter{recorder}, factory, checkpoint)}
}
func delegationWire(from, to any) error {
	raw, err := json.Marshal(from)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, to)
}
func (c *DelegationConsumer) Close() { c.core.Close() }
func (c *DelegationConsumer) ProjectPolicyChanged(project string, p DelegationPolicySnapshot) {
	var in delegate.PolicySnapshot
	if delegationWire(p, &in) == nil {
		c.core.ProjectPolicyChanged(project, in)
	}
}
func (c *DelegationConsumer) PolicyRevision(id Identity) DelegationRevision {
	return DelegationRevision{c.core.PolicyRevision(integration.Identity(id))}
}
func (c *DelegationConsumer) Register(ctx context.Context, r DelegationRegistration) error {
	var in delegate.Registration
	if err := delegationWire(r, &in); err != nil {
		return err
	}
	return c.core.Register(ctx, in)
}
func (c *DelegationConsumer) Profiles() []DelegationResolvedProfile {
	out := []DelegationResolvedProfile{}
	_ = delegationWire(c.core.Profiles(), &out)
	return out
}
func (c *DelegationConsumer) Get(id Identity, p DelegationPolicySnapshot) DelegationView {
	var in delegate.PolicySnapshot
	_ = delegationWire(p, &in)
	var out DelegationView
	_ = delegationWire(c.core.Get(integration.Identity(id), in), &out)
	return out
}
func (c *DelegationConsumer) SetOverride(ctx context.Context, id Identity, enabled *bool) error {
	return c.core.SetOverride(ctx, integration.Identity(id), enabled)
}
func (c *DelegationConsumer) EmergencyStop(ctx context.Context, enabled bool) error {
	return c.core.EmergencyStop(ctx, enabled)
}
func (c *DelegationConsumer) CaptureLaunch(id Identity, fresh bool, configured bool) (PreparedDelegationLaunch, bool) {
	snapshot, ok := c.core.CaptureLaunch(integration.Identity(id), fresh, configured)
	return PreparedDelegationLaunch{snapshot}, ok
}
func (c *DelegationConsumer) StoreLaunch(ctx context.Context, p PreparedDelegationLaunch, id Identity) error {
	return c.core.StoreLaunch(ctx, p.value, integration.Identity(id))
}
func (c *DelegationConsumer) Read(ctx context.Context, id Identity, p DelegationPolicySnapshot, r DelegationRequest, revision DelegationRevision) DelegationReply {
	var in delegate.PolicySnapshot
	_ = delegationWire(p, &in)
	result := c.core.Read(ctx, integration.Identity(id), in, delegate.Request{Question: r.Question, Paths: append([]string(nil), r.Paths...), Focus: r.Focus, OutputLimit: r.OutputLimit}, revision.value)
	var out DelegationReply
	if delegationWire(result, &out) != nil {
		out = DelegationReply{Schema: "hcg.delegate-read/1", Status: "unavailable", Reason: "integration_unavailable"}
	}
	return out
}

func (c *DelegationConsumer) MountRequested(id Identity) bool {
	return c.core.MountRequested(integration.Identity(id))
}

// RecoverDelegationReceipts retries only internal startup receipt projections.
// It cannot import caller-provided counters or issue another provider request.
func (c *DelegationConsumer) RecoverDelegationReceipts(ctx context.Context) error {
	return c.core.RecoverReceipts(ctx)
}

func (c *DelegationConsumer) LabGrant() *DelegationLabGrant {
	g := c.core.LabGrant()
	if g == nil {
		return nil
	}
	var out DelegationLabGrant
	if delegationWire(g, &out) != nil {
		return nil
	}
	return &out
}
func CheckDelegationOwnerStartup(ctx context.Context, root string) error {
	return delegate.CheckLabStartup(ctx, root)
}
