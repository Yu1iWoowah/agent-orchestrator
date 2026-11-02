package operatornative

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestFuseQualificationFailsClosed(t *testing.T) {
	const sentinel = "dL7pKGdnNz796PbbjQWNKmHXBZaB9tsX"
	valid := sentinel + string([]byte{1, 8}) + "00101100"
	for name, data := range map[string]string{
		"valid": valid, "boundary": strings.Repeat("x", 65520) + valid,
		"missing": "no fuses", "truncated": sentinel + string([]byte{1, 8}) + "0",
		"node":                    sentinel + string([]byte{1, 8}) + "10101100",
		"inspector":               sentinel + string([]byte{1, 8}) + "00111100",
		"universal-invalid-slice": valid + strings.Repeat("x", 65536) + sentinel + string([]byte{1, 8}) + "10101100",
	} {
		t.Run(name, func(t *testing.T) {
			exe := filepath.Join(t.TempDir(), "MacOS", "app")
			file := exe
			if runtime.GOOS == "darwin" {
				file = filepath.Join(filepath.Dir(filepath.Dir(exe)), "Frameworks", "Electron Framework.framework", "Versions", "A", "Electron Framework")
			}
			if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(file, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			if qualifiedFuses(exe) != (name == "valid" || name == "boundary") {
				t.Fatal("wrong fuse qualification")
			}
		})
	}
}

func TestGenericDevelopmentElectronIsNotAnAttachAnchor(t *testing.T) {
	if _, err := packagedDesktop(filepath.Join(t.TempDir(), "frontend", "daemon", "ao")); err == nil {
		t.Fatal("dev launcher qualified as packaged")
	}
}
