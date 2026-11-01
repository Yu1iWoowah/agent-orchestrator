package domain

import (
	"testing"
	"time"
)

func TestChatDeliveryRequiresExactPositiveEvidence(t *testing.T) {
	at := time.Now().UTC()
	a := ChatDeliveryAttempt{ID: "attempt", CreatedAt: at, Disposition: ChatDeliveryReserved}
	for _, tc := range []struct {
		name string
		r    ChatDeliveryReceipt
		want bool
	}{
		{"local ID", ChatDeliveryReceipt{AttemptID: "attempt", ProviderTurnID: "local", Disposition: ChatDeliveryAccepted, Source: "incomplete", ObservedAt: at}, false},
		{"frame write", ChatDeliveryReceipt{AttemptID: "attempt", ProviderRequestID: "rpc", ProviderEpoch: "epoch", Disposition: ChatDeliveryUnknown, Source: "incomplete", ObservedAt: at}, true},
		{"journal absence", ChatDeliveryReceipt{AttemptID: "attempt", ProviderRequestID: "rpc", ProviderEpoch: "epoch", Disposition: ChatDeliveryNotSent, Source: "not_forwarded", ObservedAt: at}, false},
		{"positive not forwarded", ChatDeliveryReceipt{AttemptID: "attempt", ProviderRequestID: "rpc", ProviderEpoch: "epoch", Disposition: ChatDeliveryNotSent, Source: "not_forwarded", Complete: true, ObservedAt: at}, true},
		{"matched provider", ChatDeliveryReceipt{AttemptID: "attempt", ProviderRequestID: "rpc", ProviderEpoch: "epoch", Disposition: ChatDeliveryAccepted, Source: "provider_response", Complete: true, ObservedAt: at}, true},
		{"different attempt", ChatDeliveryReceipt{AttemptID: "other", ProviderRequestID: "rpc", ProviderEpoch: "epoch", Disposition: ChatDeliveryAccepted, Source: "provider_response", Complete: true, ObservedAt: at}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := a.Accepts(tc.r); got != tc.want {
				t.Fatalf("accepts=%v want=%v", got, tc.want)
			}
		})
	}
	r := ChatDeliveryReceipt{AttemptID: "attempt", ProviderRequestID: "rpc", ProviderEpoch: "epoch", Disposition: ChatDeliveryAccepted, Source: "provider_response", Complete: true, ObservedAt: at}
	a.Disposition, a.Receipt = ChatDeliveryAccepted, &r
	for _, change := range []string{"downgrade", "request", "epoch"} {
		t.Run(change, func(t *testing.T) {
			next := r
			switch change {
			case "downgrade":
				next.Disposition = ChatDeliveryNotSent
				next.Source = "not_forwarded"
			case "request":
				next.ProviderRequestID = "other"
			case "epoch":
				next.ProviderEpoch = "other"
			}
			if a.Accepts(next) {
				t.Fatal("accepted unrelated or downgraded evidence")
			}
		})
	}
}
