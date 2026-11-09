//go:build linux

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	rh "github.com/aoagents/agent-orchestrator/backend/internal/test/reusablehandoff"
)

type directiveProvider struct {
	events  chan ports.ChatEvent
	closed  sync.Once
	harness domain.AgentHarness
}

func (p *directiveProvider) ProviderConversationID() string { return "fixture-native" }
func (p *directiveProvider) Capabilities() ports.ChatCapabilities {
	return ports.ChatCapabilities{ports.ChatCapabilityStreaming: true, ports.ChatCapabilitySteer: true, ports.ChatCapabilityApprovals: true, ports.ChatCapabilityInterrupt: true, ports.ChatCapabilityResume: true}
}
func (p *directiveProvider) Events() <-chan ports.ChatEvent { return p.events }
func (p *directiveProvider) SendTurn(context.Context, ports.ChatUserMessage) (ports.ChatTurnRef, error) {
	p.events <- ports.ChatEvent{Kind: ports.ChatEventTurnStarted, ProviderTurnID: "fixture-provider-turn"}
	return ports.ChatTurnRef{ProviderTurnID: "fixture-provider-turn"}, nil
}
func (p *directiveProvider) Steer(ctx context.Context, _ string, _ ports.ChatUserMessage) (ports.ChatTurnRef, error) {
	return ports.ChatTurnRef{ProviderTurnID: "fixture-provider-turn"}, ctx.Err()
}
func (p *directiveProvider) Interrupt(context.Context, string) error { return nil }
func (p *directiveProvider) ResolveRequest(context.Context, string, ports.ChatDecision) error {
	return nil
}
func (p *directiveProvider) Close() error { p.closed.Do(func() { close(p.events) }); return nil }

type directiveDriver struct{ provider *directiveProvider }

func (d directiveDriver) Harness() domain.AgentHarness { return d.provider.harness }
func (d directiveDriver) Probe(context.Context) (ports.ChatCapabilities, error) {
	return d.provider.Capabilities(), nil
}
func (d directiveDriver) Start(context.Context, ports.ChatStartConfig) (ports.ChatConversation, error) {
	return d.provider, nil
}
func (d directiveDriver) Resume(context.Context, ports.ChatResumeConfig) (ports.ChatConversation, error) {
	return d.provider, nil
}

type directiveRegistry struct{ driver directiveDriver }

func (r directiveRegistry) Driver(domain.AgentHarness) (ports.ChatDriver, error) {
	return r.driver, nil
}
func (r directiveRegistry) SupportsChat(domain.AgentHarness) bool { return true }

func directive(t *checks, bin string, input rh.Input) []byte {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	var snapshot map[string]any
	if err := json.Unmarshal(input.Snapshot, &snapshot); err != nil {
		t.Fatal(err)
	}
	session := fixtureValue[map[string]any](t, snapshot["session"])
	harness := domain.AgentHarness(fixtureValue[string](t, session["harness"]))
	projectID := domain.ProjectID(fixtureValue[string](t, session["projectId"]))
	if err := st.UpsertProject(ctx, domain.ProjectRecord{ID: string(projectID), Path: t.root, RegisteredAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	receiver, err := st.CreateSession(ctx, domain.SessionRecord{ProjectID: projectID, Kind: domain.SessionKind(fixtureValue[string](t, session["kind"])), Harness: harness, Mode: domain.SessionModeChat, CreatedAt: time.Now(), UpdatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	p := &directiveProvider{events: make(chan ports.ChatEvent, 32), harness: harness}
	var next atomic.Int64
	svc := chatsvc.New(chatsvc.Options{Store: st, Sessions: st, Drivers: directiveRegistry{directiveDriver{p}}, Log: slog.New(slog.DiscardHandler), NewID: func() string { return fmt.Sprintf("directive-%d", next.Add(1)) }})
	controller, err := svc.Start(ctx, chatsvc.StartConfig{SessionID: receiver.ID, ProjectID: projectID, Harness: harness, WorkspacePath: t.root})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = svc.Stop(context.Background(), receiver.ID) }()
	await := func() {
		for ctx.Err() == nil {
			rows, err := st.LoadConversationSnapshot(ctx, controller.ConversationID())
			if err != nil {
				t.Fatal(err)
			}
			for _, turn := range rows.Turns {
				if turn.ProviderTurnID == "fixture-provider-turn" {
					return
				}
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatal("fixture provider turn not bound")
	}
	if input.Directive.Active {
		if _, err := svc.Send(ctx, receiver.ID, ports.ChatUserMessage{Text: "ongoing fixture work", Origin: domain.MessageOriginHuman, ClientMessageID: "fixture-start"}); err != nil {
			t.Fatal(err)
		}
		await()
	}
	for index, msg := range input.Directive.Messages {
		result, err := svc.SteerOrSend(ctx, receiver.ID, msg, false)
		if err != nil {
			t.Fatal(err)
		}
		if result.Steered != (input.Directive.Active || index > 0) {
			t.Fatal("unexpected real steer/fallback outcome")
		}
		await()
	}
	rows, err := st.LoadConversationSnapshot(ctx, controller.ConversationID())
	if err != nil {
		t.Fatal(err)
	}
	handles := map[string]bool{}
	for _, msg := range input.Directive.Messages {
		handles[msg.ClientMessageID] = true
	}
	var output []controllersEntry
	for _, a := range rows.Activities {
		var detail map[string]any
		if err := json.Unmarshal(a.Detail, &detail); err != nil {
			t.Fatal(err)
		}
		handle, _ := detail["clientMessageId"].(string)
		if !handles[handle] {
			continue
		}
		output = append(output, controllersEntry{ID: a.ID, TurnID: a.TurnID, Sequence: a.Sequence, Activity: &a})
	}
	for _, m := range rows.Messages {
		if handles[m.ClientMessageID] {
			output = append(output, controllersEntry{ID: m.ID, TurnID: m.TurnID, Sequence: m.Sequence, Message: &m})
		}
	}
	if len(output) != len(handles) {
		t.Fatal("durable directive missing/ambiguous")
	}
	// Rebind only synthetic fixture identity to the real persisted turn; output fields stay unchanged.
	transcript := fixtureValue[map[string]any](t, snapshot["transcript"])
	turns := fixtureValue[[]any](t, transcript["turns"])
	old := fixtureValue[map[string]any](t, turns[len(turns)-1])["id"]
	actual := output[0].TurnID
	for _, v := range turns {
		turn := fixtureValue[map[string]any](t, v)
		if turn["id"] == old {
			turn["id"] = actual
			turn["state"] = "running"
			delete(turn, "completedAt")
		}
	}
	existing := fixtureValue[[]any](t, transcript["entries"])
	for _, v := range existing {
		e := fixtureValue[map[string]any](t, v)
		if e["turnId"] == old {
			e["turnId"] = actual
		}
	}
	fixtureValue[map[string]any](t, session["activity"])["state"] = "active"
	for _, entry := range output {
		raw, err := json.Marshal(entry.response())
		if err != nil {
			t.Fatal(err)
		}
		var value any
		if err := json.Unmarshal(raw, &value); err != nil {
			t.Fatal(err)
		}
		existing = append(existing, value)
		if float64(entry.Sequence) > fixtureValue[float64](t, transcript["latestSequence"]) {
			transcript["latestSequence"] = float64(entry.Sequence)
		}
	}
	transcript["entries"] = existing
	// Use the regular projector for the last real activity, or a harmless existing row for message-only fallback.
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	input.Snapshot = raw
	last := output[len(output)-1]
	if last.Activity != nil {
		input.ID = last.ID
		input.Turn = last.TurnID
		input.Sequence = last.Sequence
		a := last.Activity
		return project(t, bin, input, ports.ChatEvent{ActivityKind: a.Kind, ActivityStatus: a.Status, Summary: a.Summary, Detail: a.Detail})
	}
	return project(t, bin, input, ports.ChatEvent{})
}

type controllersEntry struct {
	ID, TurnID string
	Sequence   int64
	Activity   *domain.ConversationActivity
	Message    *domain.ConversationMessage
}

func (entry controllersEntry) response() any {
	if a := entry.Activity; a != nil {
		var detail map[string]any
		if err := json.Unmarshal(a.Detail, &detail); err != nil {
			panic(err)
		}
		return controllers.ConversationActivityResponse{Kind: "activity", ID: a.ID, TurnID: a.TurnID, Sequence: a.Sequence, ActivityKind: string(a.Kind), Status: string(a.Status), Summary: a.Summary, Detail: detail, CreatedAt: a.CreatedAt.Format(time.RFC3339)}
	}
	m := entry.Message
	return controllers.ConversationMessageResponse{Kind: "message", ID: m.ID, TurnID: m.TurnID, Sequence: m.Sequence, Revision: m.Revision, Role: string(m.Role), Origin: string(m.Origin), Text: m.Text, ClientMessageID: m.ClientMessageID, SenderSessionID: m.SenderSessionID, SenderProjectID: m.SenderProjectID, SenderDisplayName: m.SenderDisplayName, Streaming: m.Streaming, ConsumedByTurnID: m.ConsumedByTurnID}
}

func fixtureValue[T any](t *checks, raw any) T {
	value, ok := raw.(T)
	if !ok {
		t.Fatalf("invalid fixture value %T", raw)
	}
	return value
}
