package controllers

import (
	"encoding/json"
	"testing"
)

func TestSpawnAndDelegateAccountChoiceParticipatesInRequestFingerprint(t *testing.T) {
	for _, route := range []string{"session", "delegate"} {
		first := []byte(`{"clientRequestId":"same-request","projectId":"fake","agentAccountId":"account-A"}`)
		second := []byte(`{"clientRequestId":"same-request","projectId":"fake","agentAccountId":"account-B"}`)
		if route == "session" {
			var a, b SpawnSessionRequest
			if err := json.Unmarshal(first, &a); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(second, &b); err != nil {
				t.Fatal(err)
			}
			if a.AgentAccountID != "account-A" || b.AgentAccountID != "account-B" {
				t.Fatal("spawn dropped account wire field")
			}
			if clientRequestFingerprint(route, a, nil) == clientRequestFingerprint(route, b, nil) {
				t.Fatal("changed explicit account reused request fingerprint")
			}
		} else {
			var a, b DelegateTaskRequest
			if err := json.Unmarshal(first, &a); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(second, &b); err != nil {
				t.Fatal(err)
			}
			if a.AgentAccountID != "account-A" || b.AgentAccountID != "account-B" {
				t.Fatal("delegation dropped account wire field")
			}
			if clientRequestFingerprint(route, a, nil) == clientRequestFingerprint(route, b, nil) {
				t.Fatal("changed delegated account reused request fingerprint")
			}
		}
	}
}
