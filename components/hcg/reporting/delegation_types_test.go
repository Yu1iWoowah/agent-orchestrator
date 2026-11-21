package reporting

import (
	"encoding/json"
	"github.com/aoagents/agent-orchestrator/components/hcg/internal/delegate"
	"testing"
)

func TestLunaPublicProfileRoundTrip(t *testing.T) {
	for _, raw := range []string{
		`{"id":"old","prices":{"version":"v1"}}`,
		`{"id":"luna","reasoningEffort":"medium","serviceTier":"default","prices":{"cacheWriteNanoUSDPerMillion":0,"longContext":{"thresholdInputTokens":272000,"cacheWriteNanoUSDPerMillion":250000000,"inputNanoUSDPerMillion":200000000,"cachedInputNanoUSDPerMillion":20000000,"outputNanoUSDPerMillion":750000000}}}`,
	} {
		var public DelegationProfile
		if err := json.Unmarshal([]byte(raw), &public); err != nil {
			t.Fatal(err)
		}
		a, _ := json.Marshal(public)
		var internal delegate.Profile
		if err := delegationWire(public, &internal); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(internal)
		if string(a) != string(b) {
			t.Fatalf("profile boundary changed bytes: %s / %s", a, b)
		}
		var restored DelegationProfile
		if err := delegationWire(internal, &restored); err != nil {
			t.Fatal(err)
		}
		c, _ := json.Marshal(restored)
		if string(a) != string(c) {
			t.Fatal("round trip changed digest bytes")
		}
		if public.Prices.CacheWritePerMillion != nil && (internal.Prices.CacheWritePerMillion == nil || *internal.Prices.CacheWritePerMillion != 0) {
			t.Fatal("explicit zero lost")
		}
	}
}
