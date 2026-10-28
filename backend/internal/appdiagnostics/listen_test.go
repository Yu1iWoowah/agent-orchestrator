package appdiagnostics

import (
	"os"
	"path/filepath"
	"testing"
)

func TestListenersAreIsolatedByDiagnosticDataRoot(t *testing.T) {
	addresses := map[string]bool{}
	for i := 0; i < 2; i++ {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, "diagnostics"), 0o700); err != nil {
			t.Fatal(err)
		}
		ln, address, err := Listen(dir)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		if addresses[address] {
			t.Fatal("independent diagnostic roots share a listener")
		}
		addresses[address] = true
	}
}
