package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// These cases retain the three independently reproduced audit counterexamples.
func TestOperatorConsentReceiptPrecisionRoundTrip(t *testing.T) {
	for _, precision := range []struct {
		name   string
		offset time.Duration
	}{
		{"submillisecond", 123456 * time.Nanosecond},
		{"fractional_millisecond", 123456789 * time.Nanosecond},
	} {
		t.Run(precision.name, func(t *testing.T) {
			for _, accepted := range []bool{false, true} {
				name := "prepared"
				if accepted {
					name = "consumed"
				}
				t.Run(name, func(t *testing.T) {
					s, _ := operatorFixture(t)
					plan := operatorPlan(domain.ConsentHandoffRetryRestore)
					plan.ValidUntil = plan.ValidUntil.Add(precision.offset)
					receipt, err := s.PrepareOperatorAction(context.Background(), plan)
					if err != nil {
						t.Fatal(err)
					}
					ref := mintOperatorFixture(t, s, plan)
					if accepted {
						if _, err = reserveOperatorFixture(s, ref, plan, ""); err != nil {
							t.Fatal(err)
						}
					}
					rebuilt := plan
					rebuilt.ValidUntil = receipt.ValidUntil
					got, err := s.PrepareOperatorAction(context.Background(), rebuilt)
					if err != nil || got.RequestRef != receipt.RequestRef || !got.ValidUntil.Equal(receipt.ValidUntil) {
						t.Fatalf("millisecond Prepare round trip: %+v %v", got, err)
					}
					consumed, err := reserveOperatorFixture(s, ref, rebuilt, "")
					if err != nil || consumed.Replay != accepted {
						t.Fatalf("millisecond consume round trip: %+v %v", consumed, err)
					}
					assertOperatorCounts(t, s, 1, 1)
				})
			}
		})
	}
}

func TestOperatorConsentExpiredRequestAcrossRestart(t *testing.T) {
	for _, confirmed := range []bool{false, true} {
		name := "unconfirmed"
		if confirmed {
			name = "confirmed"
		}
		t.Run(name, func(t *testing.T) {
			s, path := operatorFixture(t)
			plan := operatorPlan(domain.ConsentHandoffRetryRestore)
			prepared, err := s.PrepareOperatorAction(context.Background(), plan)
			if err != nil {
				t.Fatal(err)
			}
			if confirmed {
				mintOperatorFixture(t, s, plan)
			}
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			s = openOperatorFixture(t, path)
			s.operatorEpoch = "restarted-daemon"
			s.operatorNow = func() time.Time { return plan.ValidUntil.Add(time.Minute) }
			for _, step := range []string{"get", "prepare", "get_again"} {
				var got domain.OperatorActionReceipt
				if step == "prepare" {
					got, err = s.PrepareOperatorAction(context.Background(), plan)
				} else {
					got, err = s.GetOperatorAction(context.Background(), plan.ActionKey)
				}
				if err != nil || got.State != domain.OperatorActionExpired || got.RequestRef != prepared.RequestRef {
					t.Errorf("%s expired request: %+v %v", step, got, err)
				}
			}
			assertOperatorCounts(t, s, 0, 0)
		})
	}
}

func TestOperatorConsentExpiredConsentAcrossRestart(t *testing.T) {
	for _, restart := range []string{"issuer", "daemon", "both"} {
		t.Run(restart, func(t *testing.T) {
			s, path := operatorFixture(t)
			plan := operatorPlan(domain.ConsentHandoffRetryRestore)
			prepared, err := s.PrepareOperatorAction(context.Background(), plan)
			if err != nil {
				t.Fatal(err)
			}
			original := mintOperatorFixture(t, s, plan)
			if restart == "daemon" || restart == "both" {
				if err = s.Close(); err != nil {
					t.Fatal(err)
				}
				s = openOperatorFixture(t, path)
				s.operatorEpoch = "restarted-daemon"
			}
			if restart == "issuer" || restart == "both" {
				s.operatorIssuerEpoch = "restarted-issuer"
			}
			s.operatorNow = func() time.Time { return operatorTestTime.Add(6 * time.Minute) }
			got, err := s.GetOperatorAction(context.Background(), plan.ActionKey)
			if err != nil || got.State != domain.OperatorActionExpired {
				t.Errorf("expired consent with live request: %+v %v", got, err)
			}
			if _, err = reserveOperatorFixture(s, original, plan, ""); !errors.Is(err, domain.ErrOperatorConsentUnavailable) {
				t.Fatal("expired consent accepted")
			}
			assertOperatorCounts(t, s, 0, 0)
			// The domain plan is still valid: explicit preparation and confirmation can
			// recover, while the old consent stays unusable and expiry is never extended.
			got, err = s.PrepareOperatorAction(context.Background(), plan)
			if err != nil || got.RequestRef != prepared.RequestRef || !got.ValidUntil.Equal(prepared.ValidUntil) {
				t.Fatalf("prepare live domain plan: %+v %v", got, err)
			}
			fresh := mintOperatorFixture(t, s, plan)
			if fresh == original {
				t.Fatal("old expired consent reused")
			}
			consent, err := s.qr.GetOperatorConsentByRef(context.Background(), string(fresh))
			if err != nil || consent.ExpiresAtMs > plan.ValidUntil.UnixMilli() {
				t.Fatalf("confirmation expiry=%d exceeds domain expiry=%d: %v", consent.ExpiresAtMs, plan.ValidUntil.UnixMilli(), err)
			}
			if _, err = reserveOperatorFixture(s, original, plan, ""); !errors.Is(err, domain.ErrOperatorConsentUnavailable) {
				t.Fatal("old consent revived by preparation")
			}
			if _, err = reserveOperatorFixture(s, fresh, plan, ""); err != nil {
				t.Fatal(err)
			}
			assertOperatorCounts(t, s, 1, 1)
		})
	}
}
