package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type sideSettingsRecorder interface {
	RecordSideTurnSettings(context.Context, string, string, string, domain.AgentSettingsTurnSnapshot) error
}

func TestSideSettingsSnapshotKeepsIndependentOwnerAcrossRAMRecovery(t *testing.T) {
	ctx := t.Context()
	now := time.Now().UTC()
	before := newMemorySideStore()
	_, _ = before.ClaimSideLaunch(ctx, "run", now)
	side := domain.SideConversation{PolicyVersion: sidePolicyVersion, ID: "side", SessionID: "root", MainConversationID: "main", AppRunID: "run", ProviderHostID: "btw-run-side", ProviderForkID: "child-native", Generation: "child-generation", State: "ready", Model: "child-model", Effort: "low"}
	if _, _, err := before.CreateSideConversation(ctx, side); err != nil {
		t.Fatal(err)
	}
	if _, _, err := before.ReserveSideTurn(ctx, domain.SideTurn{ID: "turn", SideID: side.ID, ClientMessageID: "request", Text: "unchanged question", CreatedAt: now}, "run"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := before.ClaimNextSideTurn(ctx, "run", nil, now); err != nil {
		t.Fatal(err)
	}
	writer, ok := any(before).(sideSettingsRecorder)
	if !ok {
		t.Fatal("side has no settings receipt before provider dispatch")
	}
	settings := domain.AgentSettings{Model: "child-model", Effort: "low", FastMode: domain.AgentFastDefault}
	owner := domain.AgentSettingsOwner{SessionID: domain.SessionID(side.ProviderHostID), ControllerGeneration: side.Generation, ProviderConversationID: side.ProviderForkID, BranchID: side.ID}
	frame := domain.AgentSettingsTurnSnapshot{State: domain.AgentSettingKnown, Source: "side", TurnID: "turn", Owner: &owner, Settings: &settings, CapturedAt: &now, Reason: "independent_launch"}
	if err := writer.RecordSideTurnSettings(ctx, side.ID, "turn", side.Generation, frame); err != nil {
		t.Fatal(err)
	}
	if err := before.SetSideTurnProviderID(ctx, side.ID, "turn", side.Generation, "native-turn"); err != nil {
		t.Fatal(err)
	}
	if err := before.UpdateSideSettings(ctx, side.ID, "later-model", "high", now); err != nil {
		t.Fatal(err)
	}
	recovery := before.export("run")
	// The real daemon/Electron boundary is JSON in RAM; it must preserve the
	// original dispatch receipt without creating any root settings revision.
	raw, err := json.Marshal(recovery)
	if err != nil {
		t.Fatal(err)
	}
	var records []SideRecoveryRecord
	if err := json.Unmarshal(raw, &records); err != nil {
		t.Fatal(err)
	}
	after := newMemorySideStore()
	_, _ = after.ClaimSideLaunch(ctx, "run", now)
	if _, err := after.recover("run", records, now); err != nil {
		t.Fatal(err)
	}
	turn, err := after.SideTurn(ctx, side.ID, "turn")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(turn)
	if strings.Contains(string(raw), "child-native") || strings.Contains(string(raw), "native-turn") {
		t.Fatal("private provider identities leaked into visible side turn")
	}
	if len(turn.SettingsAttempts) != 1 {
		t.Fatalf("side dispatch receipt lost: %+v", turn)
	}
	got := turn.SettingsAttempts[0]
	if got.Source != "side" || got.Revision != nil || got.Owner == nil || *got.Owner != owner || got.Settings == nil || *got.Settings != settings || got.ProviderTurnID != "native-turn" {
		t.Fatalf("side borrowed root/current settings: %+v", got)
	}
	turn.SettingsAttempts[0].Settings.Model = "consumer-mutation"
	unchanged, err := after.SideTurn(ctx, side.ID, "turn")
	if err != nil || unchanged.SettingsAttempts[0].Settings.Model != "child-model" {
		t.Fatal("snapshot consumer changed frozen history")
	}
	if err := after.SetSideReady(ctx, side.ID, side.Generation, side.ProviderForkID, now); err != nil {
		t.Fatal(err)
	}
	if err := after.SettleSideTurn(ctx, side.ID, "turn", side.Generation, "failed", "native-turn", "provider terminal failure", now); err != nil {
		t.Fatal(err)
	}
	if err := after.RetrySideTurn(ctx, side.ID, "turn"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := after.ClaimNextSideTurn(ctx, "run", nil, now); err != nil {
		t.Fatal(err)
	}
	frame.Attempt = 1
	selection := domain.AgentSettings{Model: "later-model", Effort: "high", FastMode: domain.AgentFastDefault}
	frame.Settings = &selection
	if err := after.RecordSideTurnSettings(ctx, side.ID, "turn", side.Generation, frame); err != nil {
		t.Fatal(err)
	}
	retried, err := after.SideTurn(ctx, side.ID, "turn")
	if err != nil || len(retried.SettingsAttempts) != 2 || retried.SettingsAttempts[0].Settings.Model != "child-model" || retried.SettingsAttempts[1].Settings.Model != "later-model" {
		t.Fatalf("retry rewrote old dispatch choice: %+v %v", retried, err)
	}
}

type settingsSideProvider struct {
	deferredSideTestConversation
	beforeSend func(ports.ChatUserMessage) error
}

func (c *settingsSideProvider) SendTurn(ctx context.Context, msg ports.ChatUserMessage) (ports.ChatTurnRef, error) {
	if err := c.beforeSend(msg); err != nil {
		return ports.ChatTurnRef{}, err
	}
	return c.deferredSideTestConversation.SendTurn(ctx, msg)
}

func TestSideSettingsReceiptPrecedesProviderIO(t *testing.T) {
	svc := New(Options{AppRunID: "run"})
	m := svc.sides
	defer m.cancel()
	ctx, cancel := context.WithCancel(m.ctx)
	defer cancel()
	now := time.Now().UTC()
	_, _ = m.store.ClaimSideLaunch(ctx, "run", now)
	side := domain.SideConversation{PolicyVersion: sidePolicyVersion, ID: "side", SessionID: "root", MainConversationID: "main", AppRunID: "run", ProviderHostID: "btw-run-side", ProviderForkID: "provider-side-1", Generation: "child-generation", State: "ready", Model: "child-model", Effort: "low"}
	if _, _, err := m.store.CreateSideConversation(ctx, side); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.store.ReserveSideTurn(ctx, domain.SideTurn{ID: "turn", SideID: side.ID, ClientMessageID: "request", Text: "unchanged question", CreatedAt: now}, "run"); err != nil {
		t.Fatal(err)
	}
	turn, _, _, err := m.store.ClaimNextSideTurn(ctx, "run", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	provider := &settingsSideProvider{deferredSideTestConversation: deferredSideTestConversation{started: make(chan string, 1), acknowledged: make(chan string, 1)}}
	provider.beforeSend = func(msg ports.ChatUserMessage) error {
		saved, err := m.store.SideTurn(ctx, side.ID, turn.ID)
		if err != nil {
			return err
		}
		if len(saved.SettingsAttempts) != 1 || msg.SettingsSnapshot == nil || msg.SettingsSnapshot.Revision != nil || msg.SettingsSnapshot.Source != "side" || msg.SettingsSnapshot.Owner == nil || msg.SettingsSnapshot.Owner.SessionID != "btw-run-side" || msg.SettingsSnapshot.Owner.ControllerGeneration != side.Generation || msg.Settings.Model != "child-model" {
			return fmt.Errorf("side settings not recorded before I/O: %+v", saved)
		}
		return nil
	}
	runtime := &sideRuntime{side: side, conv: provider, ctx: ctx, done: ctx.Done(), completed: make(chan ports.ChatEvent, 1)}
	finished := make(chan struct{})
	go func() { m.runTurn(runtime, side, turn); close(finished) }()
	select {
	case id := <-provider.started:
		runtime.completed <- ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: id, TurnState: domain.TurnStateCompleted}
	case <-time.After(time.Second):
		t.Fatal("side did not dispatch its recorded preference")
	}
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("side did not settle")
	}
	saved, err := m.store.SideTurn(ctx, side.ID, turn.ID)
	if err != nil || saved.Text != "unchanged question" || saved.State != "completed" {
		t.Fatalf("side dispatch changed or lost question: %+v %v", saved, err)
	}
}
