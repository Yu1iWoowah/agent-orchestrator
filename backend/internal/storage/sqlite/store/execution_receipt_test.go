package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	usagesvc "github.com/aoagents/agent-orchestrator/backend/internal/service/usage"
)

func TestExecutionReceiptCanonicalReplay(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	var receipts []domain.ExecutionUsageReceipt
	for _, project := range []string{"project-a", "project-b"} {
		seedProject(t, s, project)
		sess, err := s.CreateSession(ctx, sampleRecord(project))
		mustNoError(t, err)
		for _, provider := range []string{"openai", "anthropic"} {
			n, o := int64(10), int64(2)
			receipts = append(receipts, domain.ExecutionUsageReceipt{ProjectID: project, SessionID: sess.ID,
				DelegationID: "delegation", AttemptID: "attempt", BillingProvider: provider,
				ProfileID: provider + "-fixture", ProfileRevision: "v1", ProfileDigest: strings.Repeat("a", 64),
				ProjectPolicyRevision: "policy-v1", FinishReason: "stop", ModelID: "fixture-model", RequestID: "same-request",
				ObservedAt: time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC), Tokens: domain.UsageTokenMetrics{InputTokens: &n, OutputTokens: &o}})
		}
	}
	recorder := usagesvc.NewExecutionRecorder(s)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := recorder.RecordExecutionUsage(cancelled, receipts[0]); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled write=%v", err)
	}
	// The same retained receipt succeeds later; no replacement identity/price.

	for _, receipt := range receipts {
		mustNoError(t, recorder.RecordExecutionUsage(ctx, receipt))
		seq, err := s.LatestSeq(ctx)
		mustNoError(t, err)
		mustNoError(t, recorder.RecordExecutionUsage(ctx, receipt))
		replaySeq, err := s.LatestSeq(ctx)
		mustNoError(t, err)
		if replaySeq != seq {
			t.Fatal("replay emitted CDC")
		}
		changed := receipt
		changed.ModelID = "conflicting-model"
		if err := recorder.RecordExecutionUsage(ctx, changed); !errors.Is(err, domain.ErrUsageSourceEventConflict) {
			t.Fatalf("conflict: %v", err)
		}
		forged := receipt
		forged.ProjectID = "another-project"
		forged.AttemptID = "forged"
		if err := recorder.RecordExecutionUsage(ctx, forged); err == nil {
			t.Fatal("accepted cross-project receipt")
		}
	}
	reader := usagesvc.NewEventsReader(s)
	for i := 0; i < len(receipts); i += 2 {
		page, err := reader.Events(ctx, receipts[i].SessionID, 0, nil, 100)
		mustNoError(t, err)
		if len(page.Events) != 2 || len(page.Bindings) != 0 {
			t.Fatalf("events=%+v bindings=%+v", page.Events, page.Bindings)
		}
		for _, event := range page.Events {
			if event.Execution == nil || event.BindingID != 0 || event.UsageSourceID != 0 || event.Execution.ProjectID != receipts[i].ProjectID || event.Execution.ProfileRevision != "v1" || event.Execution.ProjectPolicyRevision != "policy-v1" || event.Execution.FinishReason != "stop" || event.InputTokens == nil || *event.InputTokens != 10 {
				t.Fatalf("event=%+v", event)
			}
		}
	}
	// Missing usage remains unknown; a transport error may have no request ID.
	unknown := receipts[0]
	unknown.AttemptID = "unknown"
	unknown.RequestID = ""
	unknown.UnknownReason = "transport_failed"
	unknown.Tokens = domain.UsageTokenMetrics{}
	mustNoError(t, recorder.RecordExecutionUsage(ctx, unknown))
	page, err := reader.Events(ctx, unknown.SessionID, 0, nil, 100)
	mustNoError(t, err)
	last := page.Events[len(page.Events)-1]
	if last.InputTokens != nil || last.OutputTokens != nil || last.Execution.UnknownReason != "transport_failed" {
		t.Fatalf("unknown=%+v", last)
	}
}

func TestLunaCanonicalWriteAndModeConflict(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	seedProject(t, s, "p")
	session, err := s.CreateSession(ctx, sampleRecord("p"))
	mustNoError(t, err)
	i, c, w, u, o, total := int64(100), int64(20), int64(30), int64(80), int64(10), int64(110)
	r := domain.ExecutionUsageReceipt{ProjectID: "p", SessionID: session.ID, DelegationID: "d", AttemptID: "a", BillingProvider: "openai", ProfileID: "luna", ProfileRevision: "r1", ProfileDigest: strings.Repeat("a", 64), ProjectPolicyRevision: "policy", ModelID: "gpt-6-luna", RequestID: "request", ObservedAt: time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC), RequestedServiceTier: "default", ObservedServiceTier: "default", ServiceTierStatus: "verified_default", CacheWriteInputTokens: &w, Tokens: domain.UsageTokenMetrics{InputTokens: &i, CachedInputTokens: &c, UncachedInputTokens: &u, OutputTokens: &o}, TotalTokens: &total}
	recorder := usagesvc.NewExecutionRecorder(s)
	mustNoError(t, recorder.RecordExecutionUsage(ctx, r))
	mustNoError(t, recorder.RecordExecutionUsage(ctx, r))
	w2 := int64(31)
	changed := r
	changed.CacheWriteInputTokens = &w2
	if err := recorder.RecordExecutionUsage(ctx, changed); !errors.Is(err, domain.ErrUsageSourceEventConflict) {
		t.Fatal("write conflict lost", err)
	}
	page, err := usagesvc.NewEventsReader(s).Events(ctx, session.ID, 0, nil, 100)
	mustNoError(t, err)
	raw, err := json.Marshal(page.Events[0].ProviderUsage)
	mustNoError(t, err)
	if len(page.Events) != 1 || page.Events[0].Execution.ServiceTierStatus != "verified_default" || !strings.Contains(string(raw), `"cache_write_input_tokens":30`) {
		t.Fatal(page)
	}
	r.AttemptID = "unknown"
	r.UnknownReason = "service_tier_unverified"
	r.ObservedServiceTier = ""
	r.ServiceTierStatus = "non_default"
	r.CacheWriteInputTokens = nil
	r.TotalTokens = nil
	r.Tokens = domain.UsageTokenMetrics{}
	mustNoError(t, recorder.RecordExecutionUsage(ctx, r))
	r.AttemptID = "forged"
	r.UnknownReason = ""
	if err := recorder.RecordExecutionUsage(ctx, r); err == nil {
		t.Fatal("unverified Standard accepted")
	}
}
