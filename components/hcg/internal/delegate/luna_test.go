package delegate

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

func TestLunaResponseRequiresVerifiedStandard(t *testing.T) {
	p := lunaProfile()
	for _, tier := range []string{`"default"`, `null`, `"auto"`, `"priority"`, `42`, ``} {
		t.Run(tier, func(t *testing.T) {
			envelope := map[string]any{"id": "reply", "model": p.Model, "choices": []any{map[string]any{"message": map[string]string{"content": "answer"}, "finish_reason": "stop"}}, "usage": map[string]any{"prompt_tokens": 100, "completion_tokens": 10, "total_tokens": 110, "prompt_tokens_details": map[string]any{"cached_tokens": 20, "cache_write_tokens": 30}}}
			if tier != "" {
				envelope["service_tier"] = json.RawMessage(tier)
			}
			body, _ := json.Marshal(envelope)
			out, err := projectResponse(p, WireResponse{Status: 200, Headers: map[string]string{"x-request-id": "request"}, Body: body})
			if tier == `"default"` {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || out.Receipt.UnknownReason == "" || out.Receipt.Usage.Input != nil {
				t.Fatal("unverified tier settled", out)
			}
		})
	}
}

func lunaProfile() Profile {
	p := budgetProfile("openai")
	p.Model = "gpt-6-luna"
	p.ReasoningEffort = "medium"
	p.ServiceTier = "default"
	p.ContextTokens = 1050000
	p.MaxOutputTokens = 128000
	p.OutputTokens = 2048
	w := int64(125000000)
	p.Prices.InputPerMillion = 100000000
	p.Prices.CachedInputPerMillion = 10000000
	p.Prices.OutputPerMillion = 500000000
	p.Prices.CacheWritePerMillion = &w
	p.Prices.LongContext = &LongContextPrices{272000, 200000000, 20000000, 250000000, 750000000}
	return p
}
func TestLunaPricing(t *testing.T) {
	p := lunaProfile()
	worst, err := worstCharge(p)
	if err != nil || worst != 264036002 {
		t.Fatal(worst, err)
	}
	for _, tc := range []struct{ i, r, w, o, input, cache, output int64 }{
		{272000, 20000, 30000, 10, 25950000, 200000, 5000},
		{272001, 20000, 30000, 10, 51900200, 400000, 7500},
		{100, 0, 100, 2048, 12500, 0, 1024000},
		{922000, 0, 922000, 2048, 230500000, 0, 1536000},
	} {
		r := Receipt{RequestedServiceTier: "default", ObservedServiceTier: "default", ServiceTierStatus: "verified_default", Usage: Usage{Input: &tc.i, Output: &tc.o, CachedInput: &tc.r, CacheWriteInput: &tc.w}}
		c, err := receiptCharges(budgetAttempt{Profile: p, Reserved: worst}, r)
		if err != nil || *c.Input != tc.input || *c.CachedInput != tc.cache || *c.Output != tc.output || *c.Total != tc.input+tc.cache+tc.output {
			t.Fatalf("%+v => %+v %v", tc, c, err)
		}
	}
	for _, mutate := range []func(*Profile){func(p *Profile) { p.ReasoningEffort = "none" }, func(p *Profile) { p.ServiceTier = "auto" }, func(p *Profile) { p.Prices.CacheWritePerMillion = nil }, func(p *Profile) { p.Prices.LongContext = nil }, func(p *Profile) { p.OutputTokens = 2049 }} {
		q := p
		mutate(&q)
		if _, err := worstCharge(q); err == nil {
			t.Fatal("unapproved Luna snapshot accepted")
		}
	}
}

func TestLunaUsageVectors(t *testing.T) {
	p := lunaProfile()
	for _, tc := range []struct {
		name, details, output string
		known                 bool
		write                 int64
	}{
		{"omitted_write", `{"cached_tokens":20}`, `{"reasoning_tokens":5}`, true, 0},
		{"mixed", `{"cached_tokens":20,"cache_write_tokens":30}`, `{"reasoning_tokens":5}`, true, 30},
		{"null", `{"cached_tokens":20,"cache_write_tokens":null}`, `{}`, false, 0},
		{"negative", `{"cached_tokens":20,"cache_write_tokens":-1}`, `{}`, false, 0},
		{"overlap", `{"cached_tokens":80,"cache_write_tokens":30}`, `{}`, false, 0},
		{"overflow", `{"cached_tokens":9223372036854775807,"cache_write_tokens":30}`, `{}`, false, 0},
		{"missing_cached", `{"cache_write_tokens":30}`, `{}`, false, 0},
		{"unknown_mode", `{"cached_tokens":20,"cache_write_tokens":30,"cache_write_12h_tokens":10}`, `{}`, false, 0},
		{"reasoning_subset", `{"cached_tokens":20}`, `{"reasoning_tokens":11}`, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"model":"gpt-6-luna","service_tier":"default","choices":[{"message":{"content":"answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":10,"total_tokens":110,"prompt_tokens_details":` + tc.details + `,"completion_tokens_details":` + tc.output + `}}`
			out, err := projectResponse(p, WireResponse{Status: 200, Headers: map[string]string{"x-request-id": "request"}, Body: []byte(body)})
			if tc.known {
				if err != nil || out.Receipt.Usage.CacheWriteInput == nil || *out.Receipt.Usage.CacheWriteInput != tc.write {
					t.Fatal(out, err)
				}
			} else if err == nil || out.Receipt.Usage.Input != nil || out.Receipt.UnknownReason == "" {
				t.Fatal("invalid usage accepted", out, err)
			}
		})
	}
	if _, err := priceTokens(math.MaxInt64, math.MaxInt64); err == nil {
		t.Fatal("overflow accepted")
	}
}
func TestLunaPayloadStrictAudit(t *testing.T) {
	// The real generator validates the fixed templates before this final audit.
	p := lunaProfile()
	// Exercise the final strict decoder without PAL; all mutations must fail before send.
	for _, fields := range []string{``, `,"service_tier":"auto","reasoning_effort":"medium"`, `,"service_tier":"default","reasoning_effort":"none"`, `,"service_tier":"default","reasoning_effort":null`, `,"service_tier":"default","reasoning_effort":"medium","temperature":0`} {
		calls := 0
		b := newBudget(t, 0, 0)
		bridge := budgetBridge{budget: b, profile: p, execution: budgetExecution("p", "a"), authorize: func(context.Context) bool { return true }, canonical: &receiptSink{}, actual: transportFunc(func(context.Context, WireRequest) (WireResponse, error) { calls++; return WireResponse{}, nil })}
		_, err := bridge.Do(t.Context(), WireRequest{Provider: "openai", Endpoint: p.endpoint(), Model: p.Model, ScopeDigest: textHash("scope"), Payload: []byte(`{"model":"gpt-6-luna","max_completion_tokens":2048,"messages":[{"role":"system","content":"s"},{"role":"user","content":"u"}]` + fields + `}`)})
		if err == nil || calls != 0 {
			t.Fatal("unapproved payload sent")
		}
	}
}
func TestLunaUnknownKeepsHoldAndNoRetry(t *testing.T) {
	p := lunaProfile()
	b := newBudget(t, 0, 0)
	e := budgetExecution("p", "a")
	key, err := b.reserve(t.Context(), e, p, []byte("payload"), textHash("scope"))
	if err != nil {
		t.Fatal(err)
	}
	if err = b.transition(t.Context(), key, "reserved", "dispatched"); err != nil {
		t.Fatal(err)
	}
	r := Receipt{Execution: e, ProfileID: p.ID, ProfileRevision: p.Revision, ProfileDigest: p.digest(), Provider: p.BillingProvider, Model: p.Model, UnknownReason: "service_tier_unverified", RequestedServiceTier: "default", ServiceTierStatus: "non_default"}
	sink := &receiptSink{}
	if err = b.record(t.Context(), key, r, sink); err == nil {
		t.Fatal("unknown settled")
	}
	a := readBudget(t, b).Attempts[key]
	if a.State != "unknown" || a.Charged != nil || a.Reserved != 264036002 || !a.EverDispatched || len(sink.receipts) != 1 {
		t.Fatal(a)
	}
	if _, err = b.reserve(t.Context(), budgetExecution("p", "b"), p, []byte("p"), textHash("scope")); err == nil {
		t.Fatal("hold did not freeze")
	}
}
func TestOldProfileAndReceiptBytesUnchanged(t *testing.T) {
	p := budgetProfile("openai")
	raw, _ := json.Marshal(p)
	const old = `{"prices":{"version":"price-v1","period":"2026-11","inputNanoUSDPerMillion":200000000000,"cachedInputNanoUSDPerMillion":200000000000,"outputNanoUSDPerMillion":0,"limitsCertified":true,"billableUsageCovered":true},"id":"openai-fixture","revision":"v1","billingProvider":"openai","model":"fixture-model","contextTokens":100000,"maxOutputTokens":16384,"outputTokens":128}`
	if string(raw) != old || p.digest() != textHash(old) {
		t.Fatal(string(raw))
	}
	r := budgetReceipt(budgetExecution("p", "a"), p)
	raw, _ = json.Marshal(r)
	type oldUsage struct {
		Input, Output, Total *int64
		CachedInput          *int64
		ReasoningOutput      *int64
	}
	type oldReceipt struct {
		Charges                    Charges
		ObservedAt                 time.Time
		Execution                  Execution
		ProfileID, ProfileRevision string
		ProfileDigest              string
		Provider, RequestID, Model string
		ResponseID, UnknownReason  string
		FinishReason               string
		Usage                      oldUsage
	}
	legacy, _ := json.Marshal(oldReceipt{r.Charges, r.ObservedAt, r.Execution, r.ProfileID, r.ProfileRevision, r.ProfileDigest, r.Provider, r.RequestID, r.Model, r.ResponseID, r.UnknownReason, r.FinishReason, oldUsage{r.Usage.Input, r.Usage.Output, r.Usage.Total, r.Usage.CachedInput, r.Usage.ReasoningOutput}})
	if string(raw) != string(legacy) {
		t.Fatal("old receipt digest bytes changed")
	}

	for _, field := range []string{"cacheWriteInput", "ServiceTier", "serviceTier"} {
		if strings.Contains(string(raw), field) {
			t.Fatal("new zero changed old receipt")
		}
	}
	var restored Receipt
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	replay, _ := json.Marshal(restored)
	if string(raw) != string(replay) {
		t.Fatal("old receipt replay changed bytes")
	}
}

func TestLunaPendingReceiptRetainsWriteSnapshot(t *testing.T) {
	b := newBudget(t, 0, 0)
	p := lunaProfile()
	e := budgetExecution("p", "pending-luna")
	key, err := b.reserve(t.Context(), e, p, []byte("payload"), textHash("scope"))
	if err != nil {
		t.Fatal(err)
	}
	if err = b.transition(t.Context(), key, "reserved", "dispatched"); err != nil {
		t.Fatal(err)
	}
	r := budgetReceipt(e, p)
	cached, write := int64(0), int64(30000)
	r.Usage.CachedInput = &cached
	r.Usage.CacheWriteInput = &write
	r.RequestedServiceTier = "default"
	r.ObservedServiceTier = "default"
	r.ServiceTierStatus = "verified_default"
	sink := &receiptSink{err: errors.New("canonical unavailable")}
	if err := b.record(t.Context(), key, r, sink); !errors.Is(err, ErrUsagePending) {
		t.Fatal(err)
	}
	conflict := r
	otherWrite := int64(30001)
	conflict.Usage.CacheWriteInput = &otherWrite
	if err := b.record(t.Context(), key, conflict, sink); !errors.Is(err, ErrBudgetBlocked) {
		t.Fatal("write conflict replaced pending receipt", err)
	}
	restarted := &Budget{Dir: b.Dir, Now: b.Now}
	sink.err = nil
	if err := restarted.ReplayReceipt(t.Context(), key, sink); err != nil {
		t.Fatal(err)
	}
	got := sink.receipts[0]
	if *got.Usage.CacheWriteInput != 30000 || got.ServiceTierStatus != "verified_default" || got.ProfileDigest != p.digest() || got.Charges.PriceVersion != p.Prices.Version || *got.Charges.Total != 10750000 || readBudget(t, b).Attempts[key].State != "settled" {
		t.Fatal("Luna pending snapshot changed", got)
	}
}
