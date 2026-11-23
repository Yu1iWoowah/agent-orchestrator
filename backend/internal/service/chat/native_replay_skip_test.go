package chat_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// claudeReplayTurn mirrors one turn of claude-agent-acp's history replay after
// AO's ACP normalization: an opaque turn id, the user prompt, a tool call, the
// answer, a cross-turn task list and a recovered settlement.
func claudeReplayTurn(i int, planStep string) []ports.ChatEvent {
	turn := fmt.Sprintf("acp-history-turn:%d", i)
	item := func(kind string) string { return fmt.Sprintf("%s:%s", turn, kind) }
	return []ports.ChatEvent{
		{Kind: ports.ChatEventTurnStarted, ProviderEventID: item("start"), ProviderTurnID: turn},
		{Kind: ports.ChatEventUserMessageCompleted, ProviderEventID: item("user"), ProviderTurnID: turn, ProviderItemID: item("user"), Text: fmt.Sprintf("請處理第 %d 項工作", i)},
		{Kind: ports.ChatEventActivityStarted, ProviderEventID: item("tool-start"), ProviderTurnID: turn, ProviderItemID: item("tool"),
			ActivityKind: domain.ActivityKindCommand, ActivityStatus: domain.ActivityStatusRunning, Summary: "git status"},
		{Kind: ports.ChatEventActivityCompleted, ProviderEventID: item("tool-done"), ProviderTurnID: turn, ProviderItemID: item("tool"),
			ActivityKind: domain.ActivityKindCommand, ActivityStatus: domain.ActivityStatusCompleted, Summary: "git status", Detail: json.RawMessage(`{"command":"git status"}`)},
		{Kind: ports.ChatEventPlanUpdated, ProviderEventID: item("plan:" + planStep), ProviderTurnID: turn, Plan: &domain.ConversationPlan{SpansTurns: true, Steps: []domain.ConversationPlanStep{
			{Text: "step A", Status: domain.PlanStepCompleted}, {Text: planStep, Status: domain.PlanStepInProgress}}}},
		{Kind: ports.ChatEventMessageCompleted, ProviderEventID: item("answer"), ProviderTurnID: turn, ProviderItemID: item("answer"), Text: fmt.Sprintf("第 %d 項完成。", i)},
		{Kind: ports.ChatEventTurnCompleted, ProviderEventID: item("done"), ProviderTurnID: turn, TurnState: domain.TurnStateRecovered},
	}
}

func claudeReplay(turns int) []ports.ChatEvent {
	var events []ports.ChatEvent
	for i := range turns {
		events = append(events, claudeReplayTurn(i, fmt.Sprintf("step B %d", i))...)
	}
	return events
}

type replayImportHarness struct {
	st   *store.Store
	svc  *chatsvc.Service
	conv *nativeHistoryConversation
	log  *bytes.Buffer
	logs sync.Mutex
}

func newReplayImportHarness(t *testing.T, events []ports.ChatEvent) *replayImportHarness {
	t.Helper()
	h := &replayImportHarness{st: openStore(t), conv: &nativeHistoryConversation{fakeConversation: newFakeConversation(), events: events}, log: &bytes.Buffer{}}
	var idMu sync.Mutex
	nextID := 0
	h.svc = chatsvc.New(chatsvc.Options{
		Store: h.st, Sessions: h.st,
		Reader: chatsvc.SnapshotReaderFunc(func(ctx context.Context, conversationID string) (chatsvc.ConversationRows, error) {
			rows, err := h.st.LoadConversationSnapshot(ctx, conversationID)
			if err != nil {
				return chatsvc.ConversationRows{}, err
			}
			return chatsvc.ConversationRows{Conversation: rows.Conversation, Turns: rows.Turns, Messages: rows.Messages, Activities: rows.Activities}, nil
		}),
		Drivers: fakeRegistry{driver: fakeDriver{conv: h.conv,
			start:  func(ports.ChatStartConfig) (ports.ChatConversation, error) { return h.conv, nil },
			resume: func(ports.ChatResumeConfig) (ports.ChatConversation, error) { return h.conv, nil }}},
		Log: slog.New(slog.NewTextHandler(syncWriter{&h.logs, h.log}, nil)),
		NewID: func() string {
			idMu.Lock()
			defer idMu.Unlock()
			nextID++
			return fmt.Sprintf("replay-id-%d", nextID)
		},
	})
	return h
}

type syncWriter struct {
	mu *sync.Mutex
	w  *bytes.Buffer
}

func (s syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// resume starts the controller as a resume or wake does (claude-code, Import)
// and returns it with the import's count line.
func (h *replayImportHarness) resume(t *testing.T) (*chatsvc.Controller, string) {
	t.Helper()
	h.logs.Lock()
	h.log.Reset()
	h.logs.Unlock()
	// Every resume or wake attaches a fresh provider connection. A stopped
	// controller's delivery monitor may still read the previous one.
	h.conv = &nativeHistoryConversation{fakeConversation: newFakeConversation(), events: h.conv.events}
	ctrl, err := h.svc.Start(context.Background(), chatsvc.StartConfig{
		SessionID: testSession, ProjectID: testProject, Harness: domain.HarnessClaudeCode,
		WorkspacePath: t.TempDir(), ProviderConversationID: "thread-1", HistoryMode: ports.ChatHistoryImport,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	h.logs.Lock()
	defer h.logs.Unlock()
	line := regexp.MustCompile(`(?m)^.*msg="chat native history import".*$`).FindString(h.log.String())
	if line == "" {
		t.Fatalf("no import count line in %q", h.log.String())
	}
	return ctrl, line
}

func (h *replayImportHarness) stop(t *testing.T) {
	t.Helper()
	if err := h.svc.Stop(context.Background(), testSession); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func countField(t *testing.T, line, key string) string {
	t.Helper()
	match := regexp.MustCompile(` ` + key + `=("[^"]*"|\S*)`).FindStringSubmatch(line)
	if match == nil {
		t.Fatalf("count line %q lacks %s", line, key)
	}
	return strings.Trim(match[1], `"`)
}

// A resume or wake re-reads the whole Claude history. Settled turns AO already
// holds with the same outcome and plan project nothing, and nothing in AO's
// rows changes (SDR-PR1 AC-R1).
func TestResumeSkipsSettledClaudeReplay(t *testing.T) {
	const turns = 200
	h := newReplayImportHarness(t, claudeReplay(turns))
	ctrl, _ := h.resume(t)
	before, err := h.st.LoadConversationSnapshot(context.Background(), ctrl.ConversationID())
	if err != nil || len(before.Turns) != turns {
		t.Fatalf("first import = %d turns, %v", len(before.Turns), err)
	}
	h.stop(t)

	ctrl, line := h.resume(t)
	if got := countField(t, line, "projected"); got != "" {
		t.Fatalf("settled replay still projected %q (line %q)", got, line)
	}
	if got := countField(t, line, "skipped_turns"); got != fmt.Sprint(turns) {
		t.Fatalf("skipped turns = %s, want %d (line %q)", got, turns, line)
	}
	after, err := h.st.LoadConversationSnapshot(context.Background(), ctrl.ConversationID())
	if err != nil {
		t.Fatal(err)
	}
	for name, pair := range map[string][2]any{
		"turns": {before.Turns, after.Turns}, "messages": {before.Messages, after.Messages},
		"activities": {before.Activities, after.Activities}, "latest sequence": {before.Conversation.LatestSequence, after.Conversation.LatestSequence},
	} {
		a, _ := json.Marshal(pair[0])
		b, _ := json.Marshal(pair[1])
		if !bytes.Equal(a, b) {
			t.Fatalf("%s changed on a no-op resume", name)
		}
	}
	if ctrl.State() != ports.ChatControllerReady || ctrl.PendingTurnIDForTest() != "" {
		t.Fatalf("controller after import = %s pending %q", ctrl.State(), ctrl.PendingTurnIDForTest())
	}
}

// New native content still imports: a new turn at the end, a new item in an
// old turn, or an old turn whose plan changed (SDR-PR1 AC-R2, AC-R3).
func TestResumeStillImportsNewReplayContent(t *testing.T) {
	const turns = 5
	for _, tc := range []struct {
		name    string
		replay  func() []ports.ChatEvent
		skipped string
		check   func(t *testing.T, before, after store.ConversationSnapshot)
	}{
		{"new turn at the end", func() []ports.ChatEvent { return append(claudeReplay(turns), claudeReplayTurn(turns, "step B new")...) }, "5",
			func(t *testing.T, before, after store.ConversationSnapshot) {
				if len(after.Turns) != turns+1 || len(after.Messages) != len(before.Messages)+2 {
					t.Fatalf("new turn not imported: turns %d→%d messages %d→%d", len(before.Turns), len(after.Turns), len(before.Messages), len(after.Messages))
				}
				for _, message := range after.Messages[len(before.Messages):] {
					if message.Sequence <= before.Conversation.LatestSequence {
						t.Fatalf("new message sequence %d not after %d", message.Sequence, before.Conversation.LatestSequence)
					}
				}
			}},
		{"new item in an old turn", func() []ports.ChatEvent {
			events := claudeReplay(turns)
			extra := ports.ChatEvent{Kind: ports.ChatEventActivityCompleted, ProviderEventID: "late-tool", ProviderTurnID: "acp-history-turn:2", ProviderItemID: "late-tool",
				ActivityKind: domain.ActivityKindCommand, ActivityStatus: domain.ActivityStatusCompleted, Summary: "git diff", Detail: json.RawMessage(`{"command":"git diff"}`)}
			return append(events[:3*7-1:3*7-1], append([]ports.ChatEvent{extra}, events[3*7-1:]...)...)
		}, "4",
			func(t *testing.T, before, after store.ConversationSnapshot) {
				if len(after.Activities) != len(before.Activities)+1 {
					t.Fatalf("late item not imported: activities %d→%d", len(before.Activities), len(after.Activities))
				}
			}},
		{"plan changed in an old turn", func() []ports.ChatEvent {
			events := claudeReplay(turns)
			changed := claudeReplayTurn(1, "step B rewritten")
			copy(events[7:14], changed)
			return events
		}, "4",
			func(t *testing.T, _, after store.ConversationSnapshot) {
				for _, activity := range after.Activities {
					if activity.ProviderItemID == "ao-plan-acp-history-turn:1" && !strings.Contains(string(activity.Detail), "step B rewritten") {
						t.Fatalf("changed plan not projected: %s", activity.Detail)
					}
				}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newReplayImportHarness(t, claudeReplay(turns))
			ctrl, _ := h.resume(t)
			before, err := h.st.LoadConversationSnapshot(context.Background(), ctrl.ConversationID())
			if err != nil {
				t.Fatal(err)
			}
			h.stop(t)
			h.conv.events = tc.replay()
			ctrl, line := h.resume(t)
			if got := countField(t, line, "skipped_turns"); got != tc.skipped {
				t.Fatalf("skipped turns = %s, want %s (line %q)", got, tc.skipped, line)
			}
			after, err := h.st.LoadConversationSnapshot(context.Background(), ctrl.ConversationID())
			if err != nil {
				t.Fatal(err)
			}
			tc.check(t, before, after)
		})
	}
}

// Cross-turn lists keep their order on the first resume after live work: the
// replay re-projects an older turn that gained an item, so the later turn's
// unchanged copy must be written again after it and stay current (review P2).
func TestResumeKeepsTheLatestCrossTurnListCurrent(t *testing.T) {
	h := newReplayImportHarness(t, nil)
	ctrl, _ := h.resume(t)
	plan := func(step string) *domain.ConversationPlan {
		return &domain.ConversationPlan{SpansTurns: true, Steps: []domain.ConversationPlanStep{{Text: step, Status: domain.PlanStepInProgress}}}
	}
	// Live work: turn A writes list A, then turn B writes the newer list B.
	var providerTurns []string
	for i, step := range []string{"list A", "list B"} {
		turn, err := h.svc.Send(context.Background(), testSession, ports.ChatUserMessage{Text: "live " + step, ClientMessageID: fmt.Sprintf("live-%d", i)})
		if err != nil {
			t.Fatal(err)
		}
		providerTurns = append(providerTurns, turn.ProviderTurnID)
		h.conv.fakeConversation.events <- ports.ChatEvent{Kind: ports.ChatEventPlanUpdated, ProviderEventID: "acp-host:plan-" + step, ProviderTurnID: turn.ProviderTurnID, Plan: plan(step)}
		h.conv.fakeConversation.events <- ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderEventID: "acp-host:done-" + step, ProviderTurnID: turn.ProviderTurnID, TurnState: domain.TurnStateCompleted}
		waitForProviderTurnState(t, h.st, ctrl.ConversationID(), turn.ProviderTurnID, domain.TurnStateCompleted)
	}
	planRow := func(snapshot store.ConversationSnapshot, providerTurn string) string {
		for _, activity := range snapshot.Activities {
			if activity.ProviderItemID == "ao-plan-"+providerTurn {
				return string(activity.Detail)
			}
		}
		t.Fatalf("no plan row for %s", providerTurn)
		return ""
	}
	live, err := h.st.LoadConversationSnapshot(context.Background(), ctrl.ConversationID())
	if err != nil || strings.Contains(planRow(live, providerTurns[1]), `"superseded"`) {
		t.Fatalf("live list B is not current: %v", err)
	}
	h.stop(t)

	// The history replay uses its own ids; turn A gained a tool item.
	a, b := providerTurns[0], providerTurns[1]
	h.conv.events = []ports.ChatEvent{
		{Kind: ports.ChatEventTurnStarted, ProviderEventID: "acp-history:start-a", ProviderTurnID: a},
		{Kind: ports.ChatEventActivityCompleted, ProviderEventID: "acp-history:tool-a", ProviderTurnID: a, ProviderItemID: "acp-history:tool-a",
			ActivityKind: domain.ActivityKindCommand, ActivityStatus: domain.ActivityStatusCompleted, Summary: "git status", Detail: json.RawMessage(`{"command":"git status"}`)},
		{Kind: ports.ChatEventPlanUpdated, ProviderEventID: "acp-history:plan-a", ProviderTurnID: a, Plan: plan("list A")},
		{Kind: ports.ChatEventTurnCompleted, ProviderEventID: "acp-history:done-a", ProviderTurnID: a, TurnState: domain.TurnStateCompleted},
		{Kind: ports.ChatEventTurnStarted, ProviderEventID: "acp-history:start-b", ProviderTurnID: b},
		{Kind: ports.ChatEventPlanUpdated, ProviderEventID: "acp-history:plan-b", ProviderTurnID: b, Plan: plan("list B")},
		{Kind: ports.ChatEventTurnCompleted, ProviderEventID: "acp-history:done-b", ProviderTurnID: b, TurnState: domain.TurnStateCompleted},
	}
	ctrl, _ = h.resume(t)
	after, err := h.st.LoadConversationSnapshot(context.Background(), ctrl.ConversationID())
	if err != nil {
		t.Fatal(err)
	}
	if detail := planRow(after, b); strings.Contains(detail, `"superseded"`) {
		t.Fatalf("resume made the older list current; list B = %s", detail)
	}
	if detail := planRow(after, a); !strings.Contains(detail, `"superseded":true`) {
		t.Fatalf("older list A not superseded after resume: %s", detail)
	}
}

func waitForProviderTurnState(t *testing.T, st *store.Store, conversationID, providerTurn string, state domain.TurnState) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		snapshot, err := st.LoadConversationSnapshot(context.Background(), conversationID)
		if err != nil {
			t.Fatal(err)
		}
		for _, turn := range snapshot.Turns {
			if turn.ProviderTurnID == providerTurn && turn.State == state {
				return
			}
		}
	}
	t.Fatalf("turn %s never reached %s", providerTurn, state)
}
