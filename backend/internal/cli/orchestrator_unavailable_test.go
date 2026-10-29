package cli

import (
	"strings"
	"testing"
)

func TestReplacementCommandsAreNotExposedDuringPreparationStage(t *testing.T) {
	out, _, err := executeCLI(t, Deps{}, "orchestrator", "--help")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"replace", "replacement"} {
		if strings.Contains(out, name) {
			t.Fatalf("replacement command exposed in help: %s", out)
		}
	}
	if !strings.Contains(out, "ls") {
		t.Fatalf("existing orchestrator command lost: %s", out)
	}
}
