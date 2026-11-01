package operatornative

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"runtime"
)

// Electron's v1 fuse wire, shared with @electron/fuses. Fail closed on an
// unknown wire. Read bounded chunks; never load a complete Electron image.
func executableImage(executable string) string {
	file := executable
	if runtime.GOOS == "darwin" {
		file = filepath.Join(filepath.Dir(filepath.Dir(executable)), "Frameworks", "Electron Framework.framework", "Versions", "A", "Electron Framework")
	}
	return file
}

func qualifiedFuses(executable string) bool {
	file := executableImage(executable)
	f, err := os.Open(file)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	const sentinel = "dL7pKGdnNz796PbbjQWNKmHXBZaB9tsX"
	buffer := make([]byte, 65536)
	carry := []byte{}
	seen := false
	for {
		n, err := f.Read(buffer)
		carry = append(carry, buffer[:n]...)
		chunk := carry
		for offset := 0; offset < len(chunk); {
			pos := bytes.Index(chunk[offset:], []byte(sentinel))
			if pos < 0 {
				break
			}
			start := offset + pos + len(sentinel)
			if len(chunk) < start+10 {
				if err != nil {
					return false
				}
				break
			}
			wire := chunk[start:]
			if wire[0] != 1 || wire[1] < 8 || wire[2] != '0' || wire[5] != '0' || wire[6] != '1' || wire[7] != '1' {
				return false
			}
			seen = true
			offset = start + 10
		}
		if err != nil {
			return err == io.EOF && seen
		}
		keep := len(sentinel) + 10
		if len(chunk) > keep {
			chunk = chunk[len(chunk)-keep:]
		}
		carry = append([]byte{}, chunk...)
	}
}
