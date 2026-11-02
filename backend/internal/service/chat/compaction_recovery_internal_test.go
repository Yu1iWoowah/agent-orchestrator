package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type compactionQuestionStore struct {
	Store
	mu        sync.Mutex
	questions []domain.AttentionQuestionRecord
	cancelErr error
	lists     atomic.Int64
}

func (s *compactionQuestionStore) CreateAttentionQuestion(_ context.Context, in domain.AttentionQuestionCreate, at time.Time) (domain.AttentionQuestionRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q := domain.AttentionQuestionRecord{ID: in.ID, OriginSessionID: in.OriginSessionID, ClientKey: in.ClientKey, State: "open", Version: 1, Sequence: int64(len(s.questions) + 1)}
	_ = json.Unmarshal(in.Payload, &q.Payload)
	s.questions = append(s.questions, q)
	return q, true, nil
}
func (s *compactionQuestionStore) ListAttentionQuestions(context.Context, domain.SessionID, domain.AttentionQuestionQuery, time.Time) (domain.AttentionQuestionPage, error) {
	s.lists.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	page := domain.AttentionQuestionPage{}
	for _, q := range s.questions {
		if q.State == "open" {
			page.Items = append(page.Items, q)
			page.Open++
		}
	}
	return page, nil
}
func (s *compactionQuestionStore) CancelAttentionQuestion(_ context.Context, _ domain.SessionID, id string, _ int64, _, _ string, _ time.Time) (domain.AttentionQuestionRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancelErr != nil {
		return domain.AttentionQuestionRecord{}, s.cancelErr
	}
	for i := range s.questions {
		if s.questions[i].ID == id {
			s.questions[i].State = "cancelled"
			return s.questions[i], nil
		}
	}
	return domain.AttentionQuestionRecord{}, domain.ErrAttentionQuestionNotFound
}

type recoveringCompactionConversation struct {
	recoveredSideCloseConversation
	compactErr error
	onCompact  func()
	epoch      string
	queries    atomic.Int64
	idle       atomic.Bool
}

func (c *recoveringCompactionConversation) OutboundProviderEpoch() string {
	if c.epoch != "" {
		return c.epoch
	}
	return "original-epoch"
}

func (c *recoveringCompactionConversation) Compact(context.Context) (ports.ChatCompactionResult, error) {
	if c.onCompact != nil {
		c.onCompact()
	}
	return ports.ChatCompactionResult{}, c.compactErr
}
func (c *recoveringCompactionConversation) ReconcileOutbound(context.Context) error {
	c.queries.Add(1)
	if c.idle.Load() {
		return nil
	}
	return ports.ErrChatDeliveryUnverified
}
func newCompactionSide(t *testing.T, conv ports.ChatConversation, questions *compactionQuestionStore) (*Service, domain.SideConversation) {
	t.Helper()
	var store Store
	if questions != nil {
		store = questions
	}
	svc := New(Options{AppRunID: "d9", Sessions: sideSessionReader{}, Store: store, NewID: uuid.NewString})
	t.Cleanup(func() { svc.sides.stopAll() })
	_, _ = svc.sides.store.ClaimSideLaunch(t.Context(), "d9", time.Now())
	side := domain.SideConversation{PolicyVersion: 1, ID: "side", SessionID: "session", MainConversationID: "main", AppRunID: "d9", ProviderHostID: "btw-d9-side", ProviderForkID: "fork", State: "opening", Generation: "g"}
	if _, _, err := svc.sides.store.CreateSideConversation(t.Context(), side); err != nil {
		t.Fatal(err)
	}
	if err := svc.sides.store.SetSideReady(t.Context(), side.ID, side.Generation, side.ProviderForkID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if !svc.sides.attach(side, conv) {
		t.Fatal("attach failed")
	}
	return svc, side
}
func TestUnsentSideCompactionDoesNotFenceNextQuestion(t *testing.T) {
	conv := &recoveringCompactionConversation{compactErr: errors.Join(ports.ErrChatCompactionNotSent, context.DeadlineExceeded), recoveredSideCloseConversation: recoveredSideCloseConversation{events: make(chan ports.ChatEvent)}}
	questions := &compactionQuestionStore{}
	svc, side := newCompactionSide(t, conv, questions)
	conv.onCompact = func() {
		// Shutdown can diagnose while the provider call is still resolving.
		if err := outboundAttention(t.Context(), questions, side.SessionID, "side:"+side.ID, false, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.CompactSideChat(t.Context(), side.SessionID, side.ID); !errors.Is(err, ports.ErrChatCompactionNotSent) {
		t.Fatal(err)
	}
	page, err := questions.ListAttentionQuestions(t.Context(), side.SessionID, domain.AttentionQuestionQuery{}, time.Now())
	if err != nil || page.Open != 0 {
		t.Fatalf("not-sent proof left stale attention: %+v %v", page, err)
	}
	records, err := svc.ExportSideChatLaunch(t.Context(), "d9")
	if err != nil || len(records) != 1 || records[0].CompactionID != "" || *records[0].OutboundUnverified {
		t.Fatalf("unsent reservation survived: %+v %v", records, err)
	}
	go svc.sides.schedule()
	if _, err := svc.SendSideQuestion(t.Context(), side.SessionID, side.ID, ports.ChatUserMessage{Text: "next", ClientMessageID: "next"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		conv.mu.Lock()
		sent := conv.sent
		conv.mu.Unlock()
		if sent == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("unsent compaction blocked next question")
		}
		time.Sleep(time.Millisecond)
	}
}
func TestUnknownSideCompactionVisibleAndRetryReconcilesAfterAttach(t *testing.T) {
	questions := &compactionQuestionStore{}
	conv := &recoveringCompactionConversation{compactErr: context.DeadlineExceeded, recoveredSideCloseConversation: recoveredSideCloseConversation{events: make(chan ports.ChatEvent)}}
	before, side := newCompactionSide(t, conv, questions)
	if _, err := before.CompactSideChat(t.Context(), side.SessionID, side.ID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	page, _ := questions.ListAttentionQuestions(t.Context(), side.SessionID, domain.AttentionQuestionQuery{}, time.Now())
	if page.Open != 1 {
		t.Fatal("unknown compaction invisible before shutdown")
	}
	records, err := before.ExportSideChatLaunch(t.Context(), "d9")
	if err != nil {
		t.Fatal(err)
	}
	original := records[0].CompactionID
	wire, _ := json.Marshal(records)
	records = nil
	if err := json.Unmarshal(wire, &records); err != nil {
		t.Fatal(err)
	}
	before.sides.cancel()
	after := New(Options{AppRunID: "d9", Sessions: sideSessionReader{}, Store: questions, NewID: uuid.NewString})
	defer after.sides.stopAll()
	_, _ = after.sides.store.ClaimSideLaunch(t.Context(), "d9", time.Now())
	recovered, err := after.sides.store.(*memorySideStore).recover("d9", records, time.Now())
	if err != nil || len(recovered) != 1 {
		t.Fatalf("recover: %+v %v", recovered, err)
	}
	var launches atomic.Int64
	live := &recoveringCompactionConversation{onCompact: func() { launches.Add(1) }, recoveredSideCloseConversation: recoveredSideCloseConversation{events: make(chan ports.ChatEvent)}}
	if !after.sides.attach(recovered[0], live) {
		t.Fatal("attach failed")
	}
	deadline := time.Now().Add(2 * time.Second)
	for live.queries.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("attach did not query original operation")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := after.CompactSideChat(t.Context(), side.SessionID, side.ID); !errors.Is(err, ErrCompactionWhileBusy) {
		t.Fatalf("unknown overwritten: %v", err)
	}
	current, _ := after.ExportSideChatLaunch(t.Context(), "d9")
	if current[0].CompactionID != original {
		t.Fatal("retry replaced original ID")
	}
	live.idle.Store(true)
	if _, err := after.CompactSideChat(t.Context(), side.SessionID, side.ID); !errors.Is(err, ErrSideCompactionReconciled) {
		t.Fatalf("reconciliation must require a new request: %v", err)
	}
	if launches.Load() != 0 {
		t.Fatal("reconciliation started another compact")
	}
	page, _ = questions.ListAttentionQuestions(t.Context(), side.SessionID, domain.AttentionQuestionQuery{}, time.Now())
	if page.Open != 0 {
		t.Fatal("settled operation kept attention open")
	}
	current, _ = after.ExportSideChatLaunch(t.Context(), "d9")
	if current[0].CompactionID != "" || *current[0].OutboundUnverified {
		t.Fatal("settled original stayed reserved")
	}
	if _, err := after.CompactSideChat(t.Context(), side.SessionID, side.ID); err != nil {
		t.Fatal(err)
	}
	if launches.Load() != 1 {
		t.Fatal("explicit new request did not start exactly one compact")
	}
}
func TestCompactionQuiescingRaceDoesNotLeavePending(t *testing.T) {
	conv := &compactSideTestConversation{}
	c := &Controller{conv: conv, state: ports.ChatControllerReady}
	c.sendMu.Lock()
	result := make(chan error, 1)
	go func() { _, err := c.Compact(t.Context()); result <- err }()
	c.setOutboundQuiescing(true)
	c.sendMu.Unlock()
	if err := <-result; !errors.Is(err, ports.ErrChatShutdownNotSettled) {
		t.Fatal(err)
	}
	if c.busy() || conv.compactions != 0 {
		t.Fatal("rejected compaction left busy state or reached provider")
	}
	c.setOutboundQuiescing(false)
	if c.busy() {
		t.Fatal("quiescing poisoned future dispatch")
	}
}

type backgroundCompactionCheck struct {
	ports.ChatConversation
	queries int
	settled bool
}

func (c *backgroundCompactionCheck) OutboundProviderEpoch() string { return "background-epoch" }

func (c *backgroundCompactionCheck) QueryDeliveryAttempt(context.Context, ports.ChatDeliveryLookup) (domain.ChatDeliveryReceipt, error) {
	return domain.ChatDeliveryReceipt{}, ports.ErrChatDeliveryUnverified
}

func (c *backgroundCompactionCheck) FenceAndFlushOutbound(context.Context, []ports.ChatDeliveryLookup) ([]domain.ChatDeliveryReceipt, error) {
	if c.settled {
		return nil, nil
	}
	return nil, ports.ErrChatDeliveryUnverified
}
func (c *backgroundCompactionCheck) ReconcileOutbound(context.Context) error {
	c.queries++
	if c.settled {
		return nil
	}
	return ports.ErrChatDeliveryUnverified
}

func TestSuccessfulCustodySurvivesAttentionCleanupFailure(t *testing.T) {
	for _, mode := range []string{"background", "shutdown", "compact-preflight"} {
		t.Run(mode, func(t *testing.T) {
			var logs bytes.Buffer
			questions := &compactionQuestionStore{cancelErr: errors.New("private storage sentinel")}
			conv := &backgroundCompactionCheck{settled: true}
			c := &Controller{conv: conv, store: questions, now: time.Now, after: time.After, log: slog.New(slog.NewTextHandler(&logs, nil))}
			if err := c.outboundAttention(t.Context(), false); err != nil {
				t.Fatal(err)
			}
			var err error
			if mode == "compact-preflight" {
				c.compactionPending = true
				err = c.PreflightShutdown(t.Context())
			} else {
				_, err = c.flushOutbound(t.Context(), conv, nil, mode == "shutdown")
			}
			if err != nil || c.compactionPending {
				t.Fatalf("diagnostic cleanup invalidated custody: %v", err)
			}
			if !strings.Contains(logs.String(), "CHAT_ATTENTION_CLEANUP_FAILED") || strings.Contains(logs.String(), "private storage sentinel") {
				t.Fatalf("cleanup diagnosis is missing or unsafe: %s", logs.String())
			}
			questions.mu.Lock()
			questions.cancelErr = nil
			questions.mu.Unlock()
			if _, err := c.flushOutbound(t.Context(), conv, nil, false); err != nil {
				t.Fatal(err)
			}
			page, err := questions.ListAttentionQuestions(t.Context(), "", domain.AttentionQuestionQuery{}, time.Now())
			if err != nil || page.Open != 0 {
				t.Fatalf("cleanup did not converge after recovery: %+v %v", page, err)
			}
		})
	}
}
func TestBackgroundHibernateDoesNotPromptOrReadNativeHistory(t *testing.T) {
	conv := &backgroundCompactionCheck{}
	questions := &compactionQuestionStore{}
	c := &Controller{conv: conv, store: questions, now: time.Now}
	for i := 0; i < 4; i++ {
		if _, err := c.flushOutbound(t.Context(), conv, nil, false); err == nil {
			t.Fatal("unknown background flush passed")
		}
	}
	if conv.queries != 0 || questions.lists.Load() != 0 {
		t.Fatalf("background scan queried history or prompted: %d %d", conv.queries, questions.lists.Load())
	}
	// A successful background flush may withdraw an existing foreground
	// diagnosis, even though a failed scan must never create one.
	if err := c.outboundAttention(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	conv.settled = true
	if _, err := c.flushOutbound(t.Context(), conv, nil, false); err != nil {
		t.Fatal(err)
	}
	page, err := questions.ListAttentionQuestions(t.Context(), "", domain.AttentionQuestionQuery{}, time.Now())
	if err != nil || page.Open != 0 {
		t.Fatalf("settled background flush left stale attention: %+v %v", page, err)
	}
}
func TestSideAttentionCleanupFailureStillAnnouncesAndDeletesFork(t *testing.T) {
	questions := &compactionQuestionStore{}
	conv := &recoveringCompactionConversation{recoveredSideCloseConversation: recoveredSideCloseConversation{events: make(chan ports.ChatEvent)}}
	svc, side := newCompactionSide(t, conv, questions)
	svc.stopProviderHost = func(context.Context, domain.SessionID) error { return nil }
	deleter := &sideCleanupTestConversation{}
	svc.controllers[side.SessionID] = &Controller{conv: deleter}
	if err := outboundAttention(t.Context(), questions, side.SessionID, "side:"+side.ID, false, time.Now()); err != nil {
		t.Fatal(err)
	}
	questions.mu.Lock()
	questions.cancelErr = errors.New("cancel storage unavailable")
	questions.mu.Unlock()
	_, changed, unsubscribe, err := svc.WatchSideChat(t.Context(), side.SessionID, side.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer unsubscribe()
	select {
	case <-changed:
	default:
	}
	select {
	case <-svc.sides.wake:
	default:
	}
	if err := svc.CloseIndependentSideChat(t.Context(), side.SessionID, side.ID); err != nil {
		t.Fatalf("diagnostic cancellation broke cleanup: %v", err)
	}
	if !conv.closed || len(deleter.deleted) != 1 || deleter.deleted[0] != side.ProviderForkID {
		t.Fatalf("native cleanup skipped: %v %v", conv.closed, deleter.deleted)
	}
	select {
	case <-changed:
	default:
		t.Fatal("close was not announced")
	}
	select {
	case <-svc.sides.wake:
	default:
		t.Fatal("scheduler not signalled")
	}
}

func TestReplacementSideEpochCannotRetireOriginalCompaction(t *testing.T) {
	questions := &compactionQuestionStore{}
	original := &recoveringCompactionConversation{compactErr: context.DeadlineExceeded, recoveredSideCloseConversation: recoveredSideCloseConversation{events: make(chan ports.ChatEvent)}}
	before, side := newCompactionSide(t, original, questions)
	if _, err := before.CompactSideChat(t.Context(), side.SessionID, side.ID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	records, err := before.ExportSideChatLaunch(t.Context(), "d9")
	if err != nil {
		t.Fatal(err)
	}
	if records[0].CompactionEpoch != "original-epoch" {
		t.Fatal("original host epoch not persisted")
	}
	originalID := records[0].CompactionID
	wire, _ := json.Marshal(records)
	records = nil
	if err := json.Unmarshal(wire, &records); err != nil {
		t.Fatal(err)
	}
	before.sides.cancel()
	after := New(Options{AppRunID: "d9", Sessions: sideSessionReader{}, Store: questions})
	defer after.sides.stopAll()
	_, _ = after.sides.store.ClaimSideLaunch(t.Context(), "d9", time.Now())
	recovered, err := after.sides.store.(*memorySideStore).recover("d9", records, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	replacement := &recoveringCompactionConversation{epoch: "replacement-epoch", recoveredSideCloseConversation: recoveredSideCloseConversation{events: make(chan ports.ChatEvent)}}
	replacement.idle.Store(true)
	if !after.sides.attach(recovered[0], replacement) {
		t.Fatal("attach failed")
	}
	if _, err := after.CompactSideChat(t.Context(), side.SessionID, side.ID); !errors.Is(err, ErrCompactionWhileBusy) {
		t.Fatalf("replacement host idle cleared old action: %v", err)
	}
	replacement.events <- ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, CompactionID: originalID, ProviderTurnID: "new-host-turn", TurnState: domain.TurnStateCompleted}
	replacement.events <- ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "marker", TurnState: domain.TurnStateCompleted}
	current, _ := after.ExportSideChatLaunch(t.Context(), "d9")
	if current[0].CompactionID != originalID || current[0].CompactionEpoch != "original-epoch" || !*current[0].OutboundUnverified {
		t.Fatal("replacement host event erased original owner")
	}
	if replacement.queries.Load() != 0 {
		t.Fatal("queried replacement host as original evidence")
	}
	if err := after.StopAll(t.Context()); !errors.Is(err, ports.ErrChatShutdownNotSettled) || replacement.closed {
		t.Fatalf("replacement host falsely settled stop: %v", err)
	}
	page, _ := questions.ListAttentionQuestions(t.Context(), side.SessionID, domain.AttentionQuestionQuery{}, time.Now())
	if page.Open != 1 {
		t.Fatal("replacement host lost visible recovery exit")
	}
}
