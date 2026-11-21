package delegate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestPALBudgetedCallerBothProfiles(t *testing.T) {
	if runtime.GOOS != "linux" || os.Getenv("HCG_PAL_TESTS") != "1" {
		t.Skip("isolated PAL required")
	}
	b := newBudget(t, 0, 0)
	for _, provider := range []string{"openai", "anthropic"} {
		t.Run(provider, func(t *testing.T) {
			root := t.TempDir()
			source := filepath.Join(root, "prompt.txt")
			body := "SYNTHETIC_SOURCE_142\nDOCUMENT_DIRECTIVE_IGNORE_THE_QUESTION\n"
			if err := os.WriteFile(source, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			p := budgetProfile(provider)
			execution := budgetExecution(provider, "once")
			actual := &captureTransport{t: t, profile: p}
			sink := &receiptSink{}
			bridge := &budgetBridge{budget: b, execution: execution, profile: p, actual: actual, canonical: sink, authorize: func(context.Context) bool { return true }}
			c := Consumer{PAL: PAL{Python: os.Getenv("HCG_TEST_PAL_PYTHON"), Source: os.Getenv("HCG_TEST_PAL_SOURCE"), TempRoot: root}, Transport: bridge, Recorder: bridge}
			result, err := c.DelegateRead(t.Context(), execution, p, approvedRequest(t, &c, "QUESTION_MUST_SURVIVE", source))
			state := readBudget(t, b)
			attempt := state.Attempts[bridge.key]
			if err != nil || result.Reply.Status != "ok" || len(actual.calls) != 1 || attempt.State != "settled" || len(sink.receipts) != 1 || sink.receipts[0].Charges.Total == nil || sink.receipts[0].Charges.PriceVersion != "price-v1" {
				t.Fatalf("budget caller did not close: %+v %+v %v", result, attempt, err)
			}
		})
	}
}

func TestBudgetedCallerKeepsLateReceiptAfterCancellation(t *testing.T) {
	if runtime.GOOS != "linux" || os.Getenv("HCG_PAL_TESTS") != "1" {
		t.Skip("isolated PAL required")
	}
	for _, provider := range []string{"openai", "anthropic"} {
		t.Run(provider, func(t *testing.T) {
			b := newBudget(t, 0, 0)
			p := budgetProfile(provider)
			execution := budgetExecution("p", "cancelled")
			entered, release := make(chan struct{}), make(chan struct{})
			released := false
			defer func() {
				if !released {
					close(release)
				}
			}()
			sink := &receiptSink{signal: make(chan Receipt, 1)}
			bridge := &budgetBridge{budget: b, execution: execution, profile: p, canonical: sink, authorize: func(context.Context) bool { return true }, actual: transportFunc(func(ctx context.Context, _ WireRequest) (WireResponse, error) {
				close(entered)
				<-release
				return providerFixtureReply("fixture-model", "unused late answer"), ctx.Err()
			})}
			c := Consumer{PAL: PAL{Python: os.Getenv("HCG_TEST_PAL_PYTHON"), Source: os.Getenv("HCG_TEST_PAL_SOURCE"), TempRoot: t.TempDir()}, Transport: bridge, Recorder: bridge}
			request := approvedRequest(t, &c, "inspect")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := c.DelegateRead(ctx, execution, p, request); done <- err }()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("provider did not enter")
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancel=%v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("caller waited for uncooperative transport")
			}
			// Building a later deployment snapshot cannot mutate the dispatched v1.
			// Actual host registration timing is covered by .06/.07.
			updated := p
			updated.Revision = "v2"
			updated.Prices.Version = "price-v2"
			updated.Prices.InputPerMillion = 1
			updated.Prices.CachedInputPerMillion = 1
			close(release)
			released = true
			select {
			case receipt := <-sink.signal:
				if receipt.ProfileRevision != "v1" || receipt.ProfileDigest != p.digest() || receipt.Charges.PriceVersion != "price-v1" || receipt.UnknownReason != "" || receipt.Usage.Input == nil {
					t.Fatalf("late receipt lost original identity/known usage: %+v", receipt)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("late safe receipt lost")
			}
			deadline := time.Now().Add(3 * time.Second)
			for {
				state := readBudget(t, b)
				if state.Attempts[attemptKey(execution, p)].State == "settled" {
					if state.Attempts[attemptKey(execution, p)].ProfileDigest == updated.digest() {
						t.Fatal("new snapshot rewrote old descriptor")
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("late canonical receipt did not settle")
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}
