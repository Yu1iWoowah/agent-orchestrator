package automation

import (
	"context"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type recordingAccountResolver struct {
	requests []ports.AgentAccountResolutionRequest
}

func (r *recordingAccountResolver) ResolveAccount(ctx context.Context, in ports.AgentAccountResolutionRequest) (domain.ResolvedAgentAccount, error) {
	r.requests = append(r.requests, in)
	if ports.IsRemoteAccountRequest(ctx) {
		return domain.ResolvedAgentAccount{}, ports.ErrAgentAccountLocalOnly
	}
	id := in.AccountID
	if id == "" {
		id = in.ProjectConfig.RoleAccountReference(in.Role, in.Harness)
	}
	return domain.ResolvedAgentAccount{AccountID: id, Provider: domain.AgentAccountProviderCodex, Source: domain.AgentAccountSourceRole}, nil
}
func TestAutomationAccountOverrideUpdateAndInheritance(t *testing.T) {
	store := newFakeStore()
	store.projects["scheduled"] = domain.ProjectRecord{ID: "scheduled", Config: domain.ProjectConfig{Worker: domain.RoleOverride{Harness: domain.HarnessCodex, AgentAccountID: "role-A"}}}
	resolver := &recordingAccountResolver{}
	svc := New(Deps{Store: store})
	svc.SetAccountResolver(resolver)
	input := CreateInput{ProjectID: "scheduled", DisplayName: "Fake", Prompt: "Fake", Kind: domain.KindWorker, RRule: "FREQ=DAILY", Timezone: "UTC", AgentAccountID: "explicit-B"}
	created, err := svc.Create(context.Background(), input)
	if err != nil || created.AgentAccountID != "explicit-B" {
		t.Fatalf("create override: %+v %v", created, err)
	}
	c := "explicit-C"
	updated, err := svc.Update(context.Background(), created.ID, UpdateInput{AgentAccountID: &c})
	if err != nil || updated.AgentAccountID != c {
		t.Fatalf("update override: %+v %v", updated, err)
	}
	name := "Renamed"
	updated, err = svc.Update(context.Background(), created.ID, UpdateInput{DisplayName: &name})
	if err != nil || updated.AgentAccountID != c {
		t.Fatalf("omission cleared override: %+v %v", updated, err)
	}
	inherit := ""
	updated, err = svc.Update(context.Background(), created.ID, UpdateInput{AgentAccountID: &inherit})
	if err != nil || updated.AgentAccountID != "" {
		t.Fatalf("clear override: %+v %v", updated, err)
	}
	last := resolver.requests[len(resolver.requests)-1]
	if last.Harness != domain.HarnessCodex || last.Role != domain.AgentAccountRoleWorker || last.ProjectConfig.Worker.AgentAccountID != "role-A" || last.AccountID != "" {
		t.Fatalf("inherited selection inputs: %+v", last)
	}
	for _, id := range []string{"private-A", ""} {
		input.AgentAccountID = id
		if _, err := svc.Create(ports.WithRemoteAccountRequest(context.Background()), input); err == nil {
			t.Fatal("LAN automation selected a local account")
		}
	}
}
