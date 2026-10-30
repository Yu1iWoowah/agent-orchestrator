package codex

import (
	"reflect"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestTUISettingsUsesQualifiedNativeTierBeforeLegacyFlags(t *testing.T) {
	for _, tier := range []string{"catalog-fast", "native-fast", "default"} {
		var args []string
		appendFastTierFlag(&args, domain.AgentLaunchOptions{NativeServiceTier: tier, FastMode: true})
		want := []string{"-c", "service_tier=" + codexTOMLConfigString(tier)}
		if !reflect.DeepEqual(args, want) {
			t.Fatalf("qualified tier %q became %q", tier, args)
		}
	}
}
