//go:build windows

package appdiagnostics

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"path/filepath"
	"strings"

	"github.com/Microsoft/go-winio"
)

// Listen uses a data-root-specific pipe with the creating owner and SYSTEM only.
func Listen(dataDir string) (net.Listener, string, error) {
	absolute, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, "", err
	}
	hash := sha256.Sum256([]byte(strings.ToLower(filepath.Clean(absolute))))
	name := `\\.\pipe\ao-app-diagnostics-` + hex.EncodeToString(hash[:16])
	ln, err := winio.ListenPipe(name, &winio.PipeConfig{SecurityDescriptor: "D:P(A;;GA;;;SY)(A;;GA;;;OW)"})
	return ln, name, err
}
