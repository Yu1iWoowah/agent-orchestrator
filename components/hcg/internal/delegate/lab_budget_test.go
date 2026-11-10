package delegate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func newLabBudget(t *testing.T) (*Budget, *LabGrant) {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	b := &Budget{OwnerRoot: root, Dir: filepath.Join(root, "hcg", "delegation", "budget"), Now: func() time.Time { return budgetNow }}
	g := labGrantFixture(root, "p", "p-session", budgetNow)
	if err := b.ApproveLabRun(t.Context(), g, "2026-11", map[string]ExternalSpend{"openai": {Known: true, OutsideGateOnly: true}, "anthropic": {Known: true, OutsideGateOnly: true}}); err != nil {
		t.Fatal(err)
	}
	return b, g
}

func labBudgetProfile(family string) Profile {
	p := budgetProfile(family)
	p.Prices.InputPerMillion, p.Prices.CachedInputPerMillion = 50*nanoUSD, 50*nanoUSD
	return p
}

func TestLabRunQuotaPersistsAcrossRefundSettlementAndRestart(t *testing.T) {
	for _, family := range []string{"openai", "anthropic"} {
		t.Run(family, func(t *testing.T) {
			b, g := newLabBudget(t)
			p := labBudgetProfile(family)
			for i := 0; i < 26; i++ {
				e := budgetExecution("p", fmt.Sprint(i))
				key, err := b.reserve(t.Context(), e, p, nil, textHash("synthetic-scope"))
				if err != nil {
					t.Fatal(err)
				}
				if err := b.transition(t.Context(), key, "reserved", "dispatched"); err != nil {
					t.Fatal(err)
				}
				if i%2 == 0 {
					if err := b.releaseUnsent(t.Context(), key); err != nil {
						t.Fatal(err)
					}
				} else {
					r := budgetReceipt(e, p)
					zero := int64(0)
					r.Usage.Input, r.Usage.Output, r.Usage.Total = &zero, &zero, &zero
					if err := b.record(t.Context(), key, r, &receiptSink{}); err != nil {
						t.Fatal(err)
					}
				}
				if i == 12 {
					b = &Budget{OwnerRoot: g.OwnerRoot, Dir: b.Dir, Now: func() time.Time { return budgetNow }}
				}
			}
			if err := b.ApproveLabRun(t.Context(), g, "2026-11", map[string]ExternalSpend{"openai": {Known: true, OutsideGateOnly: true}, "anthropic": {Known: true, OutsideGateOnly: true}}); err != nil {
				t.Fatal(err)
			}
			if _, err := b.reserve(t.Context(), budgetExecution("p", "27"), p, nil, textHash("synthetic-scope")); err == nil {
				t.Fatal("27th dispatch was admitted after restart/reregister/refunds")
			}
			state := readBudget(t, b)
			count := 0
			for _, a := range state.Attempts {
				if a.EverDispatched {
					count++
				}
				if a.State != "released" && a.State != "settled" {
					t.Fatal("unexpected open hold")
				}
			}
			if count != 26 {
				t.Fatalf("persistent dispatch count=%d", count)
			}
		})
	}
}

func TestLabRunLastMoneyAdmissionIsAtomic(t *testing.T) {
	b, _ := newLabBudget(t)
	p := labBudgetProfile("openai")
	e := budgetExecution("p", "first")
	key, err := b.reserve(t.Context(), e, p, nil, textHash("scope"))
	if err != nil {
		t.Fatal(err)
	}
	if err := b.transition(t.Context(), key, "reserved", "dispatched"); err != nil {
		t.Fatal(err)
	}
	if err := b.record(t.Context(), key, budgetReceipt(e, p), &receiptSink{}); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	result := make(chan string, 2)
	for i := 0; i < 2; i++ {
		go func(i int) {
			<-start
			k, err := b.reserve(t.Context(), budgetExecution("p", fmt.Sprintf("last-%d", i)), p, nil, textHash("scope"))
			if err != nil {
				k = ""
			}
			result <- k
		}(i)
	}
	close(start)
	winner := ""
	success := 0
	for i := 0; i < 2; i++ {
		if k := <-result; k != "" {
			winner = k
			success++
		}
	}
	if success != 1 {
		t.Fatalf("last $5 winners=%d", success)
	}
	if err := b.transition(t.Context(), winner, "reserved", "dispatched"); err != nil {
		t.Fatal(err)
	}
	a := readBudget(t, b).Attempts[winner]
	if err := b.record(t.Context(), winner, budgetReceipt(a.Execution, p), &receiptSink{}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.reserve(t.Context(), budgetExecution("p", "exhausted"), p, nil, textHash("scope")); err == nil {
		t.Fatal("run family exceeded $10")
	}
}

func TestLabRunLastDispatchSlotIsAtomic(t *testing.T) {
	for _, family := range []string{"openai", "anthropic"} {
		t.Run(family, func(t *testing.T) {
			b, _ := newLabBudget(t)
			p := labBudgetProfile(family)
			for i := 0; i < 25; i++ {
				k, err := b.reserve(t.Context(), budgetExecution("p", fmt.Sprint(i)), p, nil, textHash("scope"))
				if err != nil {
					t.Fatal(err)
				}
				if err := b.transition(t.Context(), k, "reserved", "dispatched"); err != nil {
					t.Fatal(err)
				}
				if err := b.releaseUnsent(t.Context(), k); err != nil {
					t.Fatal(err)
				}
			}
			var calls atomic.Int32
			start := make(chan struct{})
			done := make(chan struct{}, 2)
			for i := 0; i < 2; i++ {
				go func(i int) {
					<-start
					bridge := budgetBridge{budget: b, execution: budgetExecution("p", fmt.Sprintf("last-%d", i)), profile: p, canonical: &receiptSink{}, authorize: func(context.Context) bool { return true }, actual: transportFunc(func(context.Context, WireRequest) (WireResponse, error) {
						calls.Add(1)
						return WireResponse{}, ErrNotSent
					})}
					payload := []byte(`{"model":"fixture-model","max_completion_tokens":128,"messages":[{"role":"system","content":"fixed"},{"role":"user","content":"synthetic"}]}`)
					_, _ = bridge.Do(t.Context(), WireRequest{Provider: family, Endpoint: p.endpoint(), Model: p.Model, Payload: payload, ScopeDigest: textHash("scope")})
					done <- struct{}{}
				}(i)
			}
			close(start)
			<-done
			<-done
			if calls.Load() != 1 {
				t.Fatalf("last slot transport admissions=%d", calls.Load())
			}
			if _, err := b.reserve(t.Context(), budgetExecution("p", "27"), p, nil, textHash("scope")); err == nil {
				t.Fatal("refunded dispatched slot was reused")
			}
		})
	}
}

func TestLabRunDeadlineBindingsAndImmutableApproval(t *testing.T) {
	b, g := newLabBudget(t)
	p := labBudgetProfile("openai")
	wrong := budgetExecution("p", "wrong")
	wrong.SessionID = "unbound"
	if _, err := b.reserve(t.Context(), wrong, p, nil, textHash("scope")); err == nil {
		t.Fatal("unbound session admitted")
	}
	changed := p
	changed.Revision = "v2"
	if _, err := b.reserve(t.Context(), budgetExecution("p", "revision"), changed, nil, textHash("scope")); err == nil {
		t.Fatal("ungranted revision admitted")
	}
	key, err := b.reserve(t.Context(), budgetExecution("p", "deadline"), p, nil, textHash("scope"))
	if err != nil {
		t.Fatal(err)
	}
	b.Now = func() time.Time { return g.ExpiresAt }
	if err := b.transition(t.Context(), key, "reserved", "dispatched"); err == nil {
		t.Fatal("deadline crossed between reserve and dispatch")
	}
	if err := b.releaseUnsent(t.Context(), key); err != nil {
		t.Fatal(err)
	}
	if readBudget(t, b).Attempts[key].EverDispatched {
		t.Fatal("pre-dispatch refusal consumed a dispatch slot")
	}
	b.Now = func() time.Time { return budgetNow }
	next, _ := mergeLabGrant(nil, g)
	next.AttemptsPerProvider++
	if err := b.ApproveLabRun(t.Context(), next, "2026-11", map[string]ExternalSpend{"openai": {Known: true, OutsideGateOnly: true}, "anthropic": {Known: true, OutsideGateOnly: true}}); err == nil {
		t.Fatal("run quota top-up accepted")
	}
}
