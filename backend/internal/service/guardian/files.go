package guardian

import (
	"bufio"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
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

// headerDiscovery caches only bounded parsed metadata and filesystem identity.
// Entries expire after an hour and roots no longer in use are dropped by Collect.
type headerDiscovery struct{ cache map[artifactKey]cachedHeader }
type artifactKey struct{ root, path string }
type cachedHeader struct {
	info  os.FileInfo
	meta  header
	valid bool
	at    time.Time
}
type headerCandidate struct {
	key      artifactKey
	info     os.FileInfo
	date     string
	required bool
}
type discoveryResult struct {
	index     map[string]artifact
	conflicts map[string]bool
	gap       bool
	reads     int
}

const maxCachedHeaders = 8192
const maxDiscoveryEntries = 65536

// scan shares one inventory across all owners of the same registered roots.
// Only actual header reads consume budget; cached entries remain usable after
// that budget is exhausted. Required native roots precede recent date folders.
func (d *headerDiscovery) scan(ctx context.Context, roots []string, budget int, required []string, now time.Time) discoveryResult {
	result := discoveryResult{index: map[string]artifact{}, conflicts: map[string]bool{}}
	candidates, gap := headerCandidates(ctx, roots, required)
	result.gap = gap
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.required != b.required {
			return a.required
		}
		if a.date != b.date {
			return a.date > b.date
		}
		if !a.info.ModTime().Equal(b.info.ModTime()) {
			return a.info.ModTime().After(b.info.ModTime())
		}
		if a.key.root != b.key.root {
			return a.key.root < b.key.root
		}
		return a.key.path > b.key.path
	})
	next := make(map[artifactKey]cachedHeader)
	physical := make(map[string]os.FileInfo)
	for _, c := range candidates {
		if ctx.Err() != nil {
			result.gap = true
			break
		}
		cached, ok := d.cache[c.key]
		if !ok || now.Sub(cached.at) >= time.Hour || !os.SameFile(cached.info, c.info) || cached.info.Size() != c.info.Size() || !cached.info.ModTime().Equal(c.info.ModTime()) {
			if result.reads >= budget {
				result.gap = true
				continue
			}
			result.reads++
			cached = cachedHeader{at: now, info: c.info}
			f, err := openFile(c.key.root, c.key.path)
			if err != nil {
				result.gap = true
				continue
			}
			line, readErr := bufio.NewReaderSize(io.LimitReader(f, maxLine+1), 4096).ReadBytes('\n')
			if readErr == nil && len(line) <= maxLine {
				cached.meta, cached.valid = parseHeader(line)
			}
			info, statErr := f.Stat()
			_ = f.Close()
			if statErr != nil {
				result.gap = true
				continue
			}
			cached.info = info
		}
		if len(next) < maxCachedHeaders {
			next[c.key] = cached
		}
		if !cached.valid {
			result.gap = true
			continue
		}
		h := cached.meta
		if old, exists := result.index[h.ID]; exists {
			if old.meta != h || !os.SameFile(physical[h.ID], cached.info) {
				result.conflicts[h.ID] = true
			}
			continue
		}
		physical[h.ID] = cached.info
		result.index[h.ID] = artifact{root: c.key.root, path: c.key.path, meta: h}
	}
	d.cache = next
	return result
}

func headerCandidates(ctx context.Context, roots, required []string) ([]headerCandidate, bool) {
	var candidates []headerCandidate
	entriesLeft := maxDiscoveryEntries
	gap := false
	for _, root := range roots {
		r, err := os.OpenRoot(root)
		if err != nil {
			gap = true
			continue
		}
		var walk func(string, int)
		walk = func(dir string, depth int) {
			if ctx.Err() != nil {
				return
			}
			if depth > 8 || entriesLeft <= 0 {
				gap = true
				return
			}
			f, err := r.Open(dir)
			if err != nil {
				gap = true
				return
			}
			var entries []os.DirEntry
			for {
				chunk, err := f.ReadDir(128)
				entries = append(entries, chunk...)
				entriesLeft -= len(chunk)
				if entriesLeft <= 0 {
					gap = true
					break
				}
				if err != nil {
					if !errors.Is(err, io.EOF) {
						gap = true
					}
					break
				}
			}
			_ = f.Close()
			// Date directories are visited newest-first even when the filesystem's
			// directory enumeration is unordered and the metadata safety cap is hit.
			sort.Slice(entries, func(i, j int) bool { return entries[i].Name() > entries[j].Name() })
			for _, entry := range entries {
				if ctx.Err() != nil {
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
				info, err := r.Stat(path)
				if err != nil || !info.Mode().IsRegular() {
					gap = true
					continue
				}
				date := info.ModTime().UTC().Format("2006/01/02")
				parts := strings.Split(filepath.ToSlash(filepath.Dir(path)), "/")
				if len(parts) >= 3 {
					key := strings.Join(parts[:3], "/")
					if _, err := time.Parse("2006/01/02", key); err == nil {
						date = key
					}
				}
				needed := false
				stem := strings.TrimSuffix(entry.Name(), ".jsonl")
				for _, id := range required {
					if strings.HasSuffix(stem, id) {
						needed = true
						break
					}
				}
				candidates = append(candidates, headerCandidate{key: artifactKey{root: root, path: path}, info: info, date: date, required: needed})
			}
		}
		walk(".", 0)
		_ = r.Close()
	}
	return candidates, gap
}
