package guardian

import (
	"bufio"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const maxLine = 1 << 20

type artifact struct {
	root string
	path string
	meta header
}

// openFile confines all path resolution to an already registered root. The
// descriptor check and O_NONBLOCK reject special files even during a swap.
func openFile(root, path string) (*os.File, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	f, err := r.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, fs.ErrNotExist
	}
	return f, nil
}

// discover reads only bounded session_meta headers. No transcript tail is
// scanned until a complete lineage reaches a registered native root.
func discover(ctx context.Context, roots []string, budget int) (map[string]artifact, map[string]bool, bool) {
	index := map[string]artifact{}
	conflicts := map[string]bool{}
	remaining := budget
	gap := false
	for _, root := range roots {
		r, err := os.OpenRoot(root)
		if err != nil {
			continue
		}
		var walk func(string, int)
		walk = func(dir string, depth int) {
			if depth > 8 || remaining <= 0 {
				gap = true
				return
			}
			if ctx.Err() != nil {
				return
			}
			d, err := r.Open(dir)
			if err != nil {
				return
			}
			defer func() { _ = d.Close() }()
			for {
				entries, readErr := d.ReadDir(32)
				for _, entry := range entries {
					remaining--
					if remaining < 0 {
						gap = true
						return
					}
					if entry.Type()&os.ModeSymlink != 0 {
						continue
					}
					path := filepath.Join(dir, entry.Name())
					if entry.IsDir() {
						walk(path, depth+1)
						continue
					}
					if !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), ".jsonl") {
						continue
					}
					f, err := openFile(root, path)
					if err != nil {
						gap = true
						continue
					}
					line, err := bufio.NewReaderSize(f, maxLine+1).ReadSlice('\n')
					_ = f.Close()
					if err != nil || len(line) > maxLine {
						gap = true
						continue
					}
					h, ok := parseHeader(line)
					if !ok {
						gap = true
						continue
					}
					if old, exists := index[h.ID]; exists {
						// Duplicate copies are acceptable only with identical header facts.
						// The chosen descriptor still provides its own generation identity.
						if old.meta != h {
							conflicts[h.ID] = true
						}
						continue
					}
					index[h.ID] = artifact{root: root, path: path, meta: h}
				}
				if readErr != nil {
					if !errors.Is(readErr, io.EOF) {
						gap = true
					}
					return
				}
			}
		}
		walk(".", 0)
		_ = r.Close()
	}
	return index, conflicts, gap
}
