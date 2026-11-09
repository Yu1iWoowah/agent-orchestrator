package delegate

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

var budgetNow = time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)

func budgetProfile(provider string) Profile {
	return Profile{ID: provider + "-fixture", Revision: "v1", BillingProvider: provider, Model: "fixture-model", ContextTokens: 100000, MaxOutputTokens: 16384, OutputTokens: 128, Prices: Prices{Version: "price-v1", Period: "2026-11", InputPerMillion: 200 * nanoUSD, CachedInputPerMillion: 200 * nanoUSD, OutputPerMillion: 0, LimitsCertified: true, BillableUsageCovered: true}}
}
func newBudget(t *testing.T, openai, anthropic int64) *Budget {
	t.Helper()
	b := &Budget{Dir: filepath.Join(t.TempDir(), "budget"), Now: func() time.Time { return budgetNow }}
	if err := b.ApprovePeriod(t.Context(), "2026-11", map[string]ExternalSpend{"openai": {Known: true, OutsideGateOnly: true, Nanos: openai}, "anthropic": {Known: true, OutsideGateOnly: true, Nanos: anthropic}}); err != nil {
		t.Fatal(err)
	}
	return b
}
func budgetExecution(project, attempt string) Execution {
	return Execution{ProjectID: project, SessionID: project + "-session", DelegationID: "delegation", AttemptID: attempt, ProjectPolicyRevision: "policy-v1"}
}
func budgetReceipt(e Execution, p Profile) Receipt {
	input, output, total := int64(100000), int64(0), int64(100000)
	return Receipt{Execution: e, ProfileID: p.ID, ProfileRevision: p.Revision, ProfileDigest: p.digest(), Provider: p.BillingProvider, Model: p.Model, RequestID: "same-request", ObservedAt: budgetNow, Usage: Usage{Input: &input, Output: &output, Total: &total}}
}

type receiptSink struct {
	mu       sync.Mutex
	receipts []Receipt
	err      error
	signal   chan Receipt
}

func (s *receiptSink) Record(_ context.Context, r Receipt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.receipts = append(s.receipts, r)
	select {
	case s.signal <- r:
	default:
	}
	return nil
}
func readBudget(t *testing.T, b *Budget) budgetState {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(b.Dir, "budget.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s budgetState
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestHostBudgetConcurrentProjectsAndGlobalCap(t *testing.T) {
	b := newBudget(t, 250*nanoUSD, 350*nanoUSD)
	b2 := &Budget{Dir: b.Dir, Now: b.Now}
	var wg sync.WaitGroup
	wg.Add(2)
	keys := make([]string, 2)
	errs := make([]error, 2)
	providers := []string{"openai", "anthropic"}
	for i, instance := range []*Budget{b, b2} {
		go func(i int, instance *Budget) {
			defer wg.Done()
			keys[i], errs[i] = instance.reserve(t.Context(), budgetExecution(providers[i], "a"), budgetProfile(providers[i]), []byte("payload"), textHash("synthetic-scope"))
		}(i, instance)
	}
	wg.Wait()
	accepted := -1
	for i, err := range errs {
		if err == nil {
			if accepted >= 0 {
				t.Fatal("more than one host-wide attempt")
			}
			accepted = i
		}
	}
	if accepted < 0 {
		t.Fatalf("no reservation accepted: %v", errs)
	}
	p := budgetProfile(providers[accepted])
	e := budgetExecution(providers[accepted], "a")
	key := keys[accepted]
	if err := b.transition(t.Context(), key, "reserved", "dispatched"); err != nil {
		t.Fatal(err)
	}
	sink := &receiptSink{}
	receipt := budgetReceipt(e, p)
	if err := b.record(t.Context(), key, receipt, sink); err != nil {
		t.Fatal(err)
	}
	// The first $20 settles: provider-specific headroom still exists, but only
	// $10 remains under the one $630 cap shared by all projects/families.
	other := 1 - accepted
	if _, err := b2.reserve(t.Context(), budgetExecution("another-project", "b"), budgetProfile(providers[other]), []byte("payload"), textHash("synthetic-scope")); err == nil {
		t.Fatal("separate project/provider received another $630")
	}
	before := readBudget(t, b)
	if err := b.record(t.Context(), key, receipt, sink); err != nil {
		t.Fatal(err)
	}
	after := readBudget(t, b)
	if *before.Attempts[key].Charged != *after.Attempts[key].Charged || len(sink.receipts) != 1 {
		t.Fatal("receipt replay charged/captured twice")
	}
}
func TestBudgetProviderCapsUnknownAndMonthHold(t *testing.T) {
	for _, provider := range []string{"openai", "anthropic"} {
		t.Run(provider, func(t *testing.T) {
			openai, anthropic := int64(0), int64(0)
			if provider == "openai" {
				openai = 290 * nanoUSD
			} else {
				anthropic = 390 * nanoUSD
			}
			b := newBudget(t, openai, anthropic)
			if _, err := b.reserve(t.Context(), budgetExecution("p", "a"), budgetProfile(provider), nil, textHash("synthetic-scope")); err == nil {
				t.Fatal("provider cap bypassed")
			}
		})
	}
	b := newBudget(t, 0, 0)
	p := budgetProfile("anthropic")
	e := budgetExecution("p", "unknown")
	key, err := b.reserve(t.Context(), e, p, nil, textHash("synthetic-scope"))
	if err != nil {
		t.Fatal(err)
	}
	if err := b.transition(t.Context(), key, "reserved", "dispatched"); err != nil {
		t.Fatal(err)
	}
	receipt := budgetReceipt(e, p)
	receipt.Usage = Usage{}
	receipt.UnknownReason = "usage_missing"
	if err := b.record(t.Context(), key, receipt, &receiptSink{}); !errors.Is(err, ErrBudgetBlocked) {
		t.Fatalf("unknown=%v", err)
	}
	restarted := &Budget{Dir: b.Dir, Now: func() time.Time { return budgetNow.AddDate(0, 1, 0) }}
	if _, err := restarted.reserve(t.Context(), budgetExecution("q", "next"), budgetProfile("openai"), nil, textHash("synthetic-scope")); err == nil {
		t.Fatal("restart/month erased unknown hold")
	}
	if err := restarted.ApprovePeriod(t.Context(), "2026-12", map[string]ExternalSpend{"openai": {Known: true, OutsideGateOnly: true}, "anthropic": {Known: true, OutsideGateOnly: true}}); err == nil {
		t.Fatal("new period erased old hold")
	}
	state := readBudget(t, b)
	if state.Attempts[key].State != "unknown" || state.Attempts[key].Reserved != 20*nanoUSD {
		t.Fatalf("lost hold: %+v", state.Attempts[key])
	}
}
func TestPendingReceiptReplayUsesDispatchPrice(t *testing.T) {
	b := newBudget(t, 0, 0)
	p := budgetProfile("openai")
	e := budgetExecution("p", "pending")
	key, err := b.reserve(t.Context(), e, p, []byte("payload"), textHash("synthetic-scope"))
	if err != nil {
		t.Fatal(err)
	}
	if err := b.transition(t.Context(), key, "reserved", "dispatched"); err != nil {
		t.Fatal(err)
	}
	sink := &receiptSink{err: errors.New("canonical write failed")}
	r := budgetReceipt(e, p)
	if err := b.record(t.Context(), key, r, sink); err == nil {
		t.Fatal("lost canonical write failure")
	}
	conflicting := r
	conflicting.RequestID = "conflicting-request"
	if err := b.record(t.Context(), key, conflicting, sink); err == nil {
		t.Fatal("pending receipt conflict accepted")
	}
	retained, ok := b.pending.Load(key)
	if !ok || retained.(Receipt).RequestID != r.RequestID {
		t.Fatal("conflict overwrote retained safe receipt")
	}
	state := readBudget(t, b)
	if state.Attempts[key].State != "usage_pending" {
		t.Fatal("pending hold released")
	}
	p.Revision = "v2"
	p.Prices.InputPerMillion = nanoUSD
	p.Prices.Version = "price-v2"
	if _, err := b.reserve(t.Context(), budgetExecution("q", "other"), p, nil, textHash("synthetic-scope")); err == nil {
		t.Fatal("profile update erased pending hold")
	}
	restarted := &Budget{Dir: b.Dir, Now: b.Now}
	sink.err = nil
	if err := restarted.ReplayReceipt(t.Context(), key, sink); err != nil {
		t.Fatal(err)
	}
	state = readBudget(t, b)
	if state.Attempts[key].State != "settled" || *state.Attempts[key].Charged != 20*nanoUSD || sink.receipts[0].Charges.PriceVersion != "price-v1" || sink.receipts[0].ProfileRevision != "v1" {
		t.Fatalf("replayed at new price: %+v %+v", state.Attempts[key], sink.receipts)
	}
	r.RequestID = "conflict"
	if err := b.record(t.Context(), key, r, sink); err == nil {
		t.Fatal("conflicting receipt accepted")
	}
}
func TestBudgetUnknownAdmissionAndPreDispatchInvalidation(t *testing.T) {
	b := newBudget(t, 0, 0)
	p := budgetProfile("openai")
	for _, mutate := range []func(*Profile){func(p *Profile) { p.Prices.Version = "" }, func(p *Profile) { p.Prices.LimitsCertified = false }, func(p *Profile) { p.Prices.BillableUsageCovered = false }} {
		changed := p
		mutate(&changed)
		if _, err := b.reserve(t.Context(), budgetExecution("p", "bad"), changed, nil, textHash("synthetic-scope")); err == nil {
			t.Fatal("unknown upper bound admitted")
		}
	}
	if err := b.ApprovePeriod(t.Context(), "2026-11", map[string]ExternalSpend{"openai": {Known: false, OutsideGateOnly: true}, "anthropic": {Known: true, OutsideGateOnly: true}}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.reserve(t.Context(), budgetExecution("p", "bad"), p, nil, textHash("synthetic-scope")); err == nil {
		t.Fatal("unknown external spend admitted")
	}
	b = newBudget(t, 0, 0)
	calls := 0
	bridge := budgetBridge{budget: b, execution: budgetExecution("p", "stale"), profile: p, canonical: &receiptSink{}, authorize: func(context.Context) bool { return false }, actual: transportFunc(func(context.Context, WireRequest) (WireResponse, error) { calls++; return WireResponse{}, nil })}
	payload := []byte(`{"model":"fixture-model","max_completion_tokens":128,"messages":[{"role":"system","content":"fixed"},{"role":"user","content":"question"}]}`)
	_, err := bridge.Do(t.Context(), WireRequest{Provider: "openai", Endpoint: p.endpoint(), Model: p.Model, Payload: payload, ScopeDigest: textHash("synthetic-scope")})
	if err == nil || calls != 0 || readBudget(t, b).Attempts[bridge.key].State != "released" {
		t.Fatalf("stale dispatch reached provider: %v calls=%d", err, calls)
	}
	if err := os.WriteFile(filepath.Join(b.Dir, "budget.json"), []byte(`{"Version":1`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := b.reserve(t.Context(), budgetExecution("p", "corrupt"), p, nil, textHash("synthetic-scope")); err == nil {
		t.Fatal("corrupt budget reset")
	}
}

func TestBudgetKeepsReceiptWhenPendingSpoolFails(t *testing.T) {
	b := newBudget(t, 0, 0)
	p := budgetProfile("openai")
	e := budgetExecution("p", "disk-failed")
	key, err := b.reserve(t.Context(), e, p, nil, textHash("synthetic-scope"))
	if err != nil {
		t.Fatal(err)
	}
	if err := b.transition(t.Context(), key, "reserved", "dispatched"); err != nil {
		t.Fatal(err)
	}
	b.writePending = func(context.Context, string, []byte) error { return errors.New("synthetic disk full") }
	receipt := budgetReceipt(e, p)
	sink := &receiptSink{}
	if err := b.record(t.Context(), key, receipt, sink); err == nil {
		t.Fatal("spool failure ignored")
	}
	if _, err := b.reserve(t.Context(), budgetExecution("other", "next"), p, nil, textHash("synthetic-scope")); err == nil {
		t.Fatal("spool failure released worst hold")
	}
	if _, ok := b.pending.Load(key); !ok {
		t.Fatal("safe receipt was lost from the live daemon")
	}
	b.writePending = nil
	if err := b.ReplayReceipt(t.Context(), key, sink); err != nil {
		t.Fatal(err)
	}
	if readBudget(t, b).Attempts[key].State != "settled" || len(sink.receipts) != 1 {
		t.Fatal("retained safe receipt could not replay")
	}
}
func TestBudgetFractionalNanoRoundingIsReserved(t *testing.T) {
	p := budgetProfile("openai")
	p.ContextTokens = 10
	p.OutputTokens = 1
	p.MaxOutputTokens = 1
	p.Prices.InputPerMillion = 1
	p.Prices.CachedInputPerMillion = 1
	worst, err := worstCharge(p)
	if err != nil {
		t.Fatal(err)
	}
	r := budgetReceipt(budgetExecution("p", "round"), p)
	input, cached, output := int64(10), int64(1), int64(0)
	r.Usage.Input = &input
	r.Usage.CachedInput = &cached
	r.Usage.Output = &output
	charged, err := receiptCharges(budgetAttempt{Profile: p, Reserved: worst}, r)
	if err != nil || worst != 2 || *charged.Total != 2 {
		t.Fatalf("split rounding exceeded reservation: %d %+v %v", worst, charged, err)
	}
	r.Usage.CachedInput = nil
	charged, err = receiptCharges(budgetAttempt{Profile: p, Reserved: worst}, r)
	if err != nil || *charged.Total != 2 || charged.CachedInput != nil {
		t.Fatal("unknown cache split lost conservative rounding or invented zero")
	}

}

func TestBudgetMonthChangesBeforeDispatchAndLateSettlement(t *testing.T) {
	b := newBudget(t, 0, 0)
	p := budgetProfile("openai")
	calls := 0
	bridge := budgetBridge{budget: b, execution: budgetExecution("p", "boundary"), profile: p, canonical: &receiptSink{}, authorize: func(context.Context) bool {
		b.Now = func() time.Time { return budgetNow.AddDate(0, 1, 0) }
		return true
	}, actual: transportFunc(func(context.Context, WireRequest) (WireResponse, error) { calls++; return WireResponse{}, nil })}
	payload := []byte(`{"model":"fixture-model","max_completion_tokens":128,"messages":[{"role":"system","content":"fixed"},{"role":"user","content":"question"}]}`)
	if _, err := bridge.Do(t.Context(), WireRequest{Provider: "openai", Endpoint: p.endpoint(), Model: p.Model, Payload: payload, ScopeDigest: textHash("synthetic-scope")}); err == nil || calls != 0 || readBudget(t, b).Attempts[bridge.key].State != "released" {
		t.Fatal("old-month reservation dispatched")
	}
	b = newBudget(t, 0, 0)
	e := budgetExecution("p", "late-month")
	key, err := b.reserve(t.Context(), e, p, nil, textHash("synthetic-scope"))
	if err != nil {
		t.Fatal(err)
	}
	if err := b.transition(t.Context(), key, "reserved", "dispatched"); err != nil {
		t.Fatal(err)
	}
	b.Now = func() time.Time { return budgetNow.AddDate(0, 1, 0) }
	if err := b.record(t.Context(), key, budgetReceipt(e, p), &receiptSink{}); err != nil {
		t.Fatal(err)
	}
	if err := b.ApprovePeriod(t.Context(), "2026-12", map[string]ExternalSpend{"openai": {Known: true, OutsideGateOnly: true, Nanos: 270 * nanoUSD}, "anthropic": {Known: true, OutsideGateOnly: true}}); err != nil {
		t.Fatal(err)
	}
	p.Prices.Period = "2026-12"
	p.Revision = "v2"
	if _, err := b.reserve(t.Context(), budgetExecution("q", "new-month"), p, nil, textHash("synthetic-scope")); err == nil {
		t.Fatal("new month forgot cross-month gate spend")
	}
	b.Now = func() time.Time { return budgetNow }
	if err := b.ApprovePeriod(t.Context(), "2026-11", map[string]ExternalSpend{"openai": {Known: true, OutsideGateOnly: true}, "anthropic": {Known: true, OutsideGateOnly: true}}); err == nil {
		t.Fatal("clock rollback reset budget window")
	}
}
