package delegate

import (
	"context"
	"errors"
	"testing"
)

func TestBudgetParentProvesUnsentWithoutInventingUsage(t *testing.T) {
	b := newBudget(t, 0, 0)
	p := budgetProfile("openai")
	sink := &receiptSink{}
	bridge := budgetBridge{budget: b, execution: budgetExecution("p", "unsent-parent"), profile: p, canonical: sink, authorize: func(context.Context) bool { return true }, actual: transportFunc(func(context.Context, WireRequest) (WireResponse, error) {
		return WireResponse{}, ErrNotSent
	})}
	payload := []byte(`{"model":"fixture-model","max_completion_tokens":128,"messages":[{"role":"system","content":"fixed"},{"role":"user","content":"question"}]}`)
	_, err := bridge.Do(t.Context(), WireRequest{Provider: "openai", Endpoint: p.endpoint(), Model: p.Model, Payload: payload, ScopeDigest: textHash("synthetic-scope")})
	if !errors.Is(err, ErrNotSent) || readBudget(t, b).Attempts[bridge.key].State != "released" {
		t.Fatal("proven unsent parent refusal retained an avoidable hold")
	}
	if err := bridge.Record(t.Context(), Receipt{}); err != nil || len(sink.receipts) != 0 {
		t.Fatal("unsent refusal invented canonical provider usage")
	}
}
