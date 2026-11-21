//go:build linux

package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	rh "github.com/aoagents/agent-orchestrator/backend/internal/test/reusablehandoff"
)

// The shared test-only projector runs real Cobra and production HTTP DTOs.
// The driver gates additionally run the complete helper → producer → CLI suite.
func TestReusableHandoffWire(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal("RH wire tests require python3:", err)
	}
	bin := rh.BuildCLI(t)
	fixtures := t.TempDir()
	cmd := exec.Command(bin, "--rh-fixtures", fixtures)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Cobra fixture producer: %v\n%s", err, out)
	}
	tests := filepath.Join(rh.Repository(t), "tools/reusable-handoff/tests")
	cmd = exec.Command(python, "-I", "-B", "-m", "unittest", "discover", "-v", "-s", tests, "-p", "test_wire_contract.py")
	cmd.Env = append(os.Environ(), "RH_WIRE_FIXTURES="+fixtures)
	out, err := cmd.CombinedOutput()
	t.Logf("actual Cobra wire → Python boundary:\n%s", out)
	if err != nil {
		t.Fatalf("RH wire boundary: %v", err)
	}
}
