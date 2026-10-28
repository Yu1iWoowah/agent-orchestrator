package appdiagnostics

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/google/uuid"
)

type diskStore struct {
	writeContent func(*os.File, []byte) (int, error)
	root         *os.Root
	dropped      uint64
}

func openStore(dataDir string) (*diskStore, error) {
	name := filepath.Join(dataDir, "diagnostics")
	if err := privateDir(name); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	s := &diskStore{root: root, writeContent: (*os.File).Write}
	for _, dir := range []string{"grants", "snapshots", "audit"} {
		if err := privateDir(filepath.Join(name, dir)); err != nil {
			_ = root.Close()
			return nil, err
		}
	}
	return s, nil
}
func (s *diskStore) name(dir, id string) (string, error) {
	if dir != "grants" && dir != "snapshots" && dir != "audit" {
		return "", errors.New("invalid diagnostic directory")
	}
	if _, err := uuid.Parse(id); err != nil {
		return "", errors.New("invalid diagnostic identifier")
	}
	return filepath.Join(dir, id+".json"), nil
}
func (s *diskStore) write(dir, id string, value any) error {
	name, err := s.name(dir, id)
	if err != nil {
		return err
	}
	// Validate the directory again: a moved/replaced directory must not redirect output.
	if err := s.checkDir(dir); err != nil {
		return err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(data) > 1<<20 {
		return errors.New("diagnostic payload exceeds limit")
	}
	if info, err := s.root.Lstat(name); err == nil {
		if err := privateFile(info); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	tmp := filepath.Join(dir, uuid.NewString()+".tmp")
	f, err := s.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = s.root.Remove(tmp) }()
	if err := secureCreatedFile(f); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := s.writeContent(f, data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return s.root.Rename(tmp, name)
}
func (s *diskStore) checkDir(dir string) error {
	info, err := s.root.Lstat(dir)
	if err != nil {
		return err
	}
	if err := privateDirectoryInfo(info); err != nil {
		return err
	}
	file, err := s.root.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	return secureOpenedFile(file, true)
}
func (s *diskStore) read(dir, id string, value any) error {
	name, err := s.name(dir, id)
	if err != nil {
		return err
	}
	if err := s.checkDir(dir); err != nil {
		return err
	}
	info, err := s.root.Lstat(name)
	if err != nil {
		return err
	}
	if err := privateFile(info); err != nil {
		return err
	}
	f, err := s.root.Open(name)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if err := secureOpenedFile(f, false); err != nil {
		return err
	}
	opened, err := f.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(info, opened) {
		return errors.New("diagnostic file changed")
	}
	dec := json.NewDecoder(io.LimitReader(f, (1<<20)+1))
	dec.DisallowUnknownFields()
	if err := dec.Decode(value); err != nil {
		return err
	}
	if dec.Decode(new(any)) != io.EOF {
		return errors.New("invalid diagnostic document")
	}
	return nil
}
func (s *diskStore) remove(dir, id string) error {
	name, err := s.name(dir, id)
	if err != nil {
		return err
	}
	if err := s.checkDir(dir); err != nil {
		return err
	}
	if err := s.root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
func (s *diskStore) entries(dir string) ([]os.DirEntry, error) {
	if err := s.checkDir(dir); err != nil {
		return nil, err
	}
	f, err := s.root.Open(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return f.ReadDir(-1)
}
func (s *diskStore) removeGrant(id string) error {
	entries, err := s.entries("snapshots")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		var row Record
		key := entry.Name()
		if filepath.Ext(key) != ".json" {
			continue
		}
		key = key[:len(key)-5]
		if err := s.read("snapshots", key, &row); err != nil {
			return err
		}
		if row.GrantID == id {
			if err := s.remove("snapshots", key); err != nil {
				return err
			}
		}
	}
	return nil
}
func (s *diskStore) audit(row Audit) error {
	if err := s.write("audit", row.RequestID, row); err != nil {
		return err
	}
	return s.prune(row.At)
}

// reset never revives a credential or payload from another daemon epoch.
const retentionID = "00000000-0000-0000-0000-000000000000"

func (s *diskStore) reset(now time.Time) error {
	var summary Audit
	if err := s.read("audit", retentionID, &summary); err == nil {
		s.dropped = summary.Count
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, dir := range []string{"grants", "snapshots"} {
		entries, err := s.entries(dir)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.IsDir() {
				return errors.New("unexpected diagnostic directory")
			}
			if err := s.root.Remove(filepath.Join(dir, entry.Name())); err != nil {
				return err
			}
		}
	}
	entries, err := s.entries("audit")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) == ".tmp" {
			if err := s.root.Remove(filepath.Join("audit", entry.Name())); err != nil {
				return err
			}
			continue
		}
		key := entry.Name()
		if len(key) < 5 {
			return errors.New("invalid audit file")
		}
		var row Audit
		if err := s.read("audit", key[:len(key)-5], &row); err != nil {
			return err
		}
		if row.Outcome == "accepted" {
			row.At = now
			row.Outcome = "interrupted"
			if err := s.write("audit", row.RequestID, row); err != nil {
				return err
			}
		}
	}
	return s.prune(now)
}
func (s *diskStore) prune(now time.Time) error {
	type item struct {
		id   string
		at   time.Time
		size int64
	}
	for _, dir := range []string{"snapshots", "audit"} {
		entries, err := s.entries(dir)
		if err != nil {
			return err
		}
		items := make([]item, 0, len(entries))
		var total int64
		for _, entry := range entries {
			key := entry.Name()
			if filepath.Ext(key) != ".json" {
				return errors.New("unexpected diagnostic file")
			}
			key = key[:len(key)-5]
			if dir == "audit" && key == retentionID {
				continue
			}
			var at time.Time
			if dir == "snapshots" {
				var row Record
				if err := s.read(dir, key, &row); err != nil {
					return err
				}
				at = row.CreatedAt
			} else {
				var row Audit
				if err := s.read(dir, key, &row); err != nil {
					return err
				}
				at = row.At
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			items = append(items, item{key, at, info.Size()})
			total += info.Size()
		}
		sort.Slice(items, func(i, j int) bool { return items[i].at.Before(items[j].at) })
		count := len(items)
		for _, entry := range items {
			limit := 20
			ttl := 24 * time.Hour
			if dir == "audit" {
				limit = 9999
				ttl = 7 * 24 * time.Hour
			}
			if now.Before(entry.at.Add(ttl)) && count <= limit && (dir != "snapshots" || total <= 20<<20) {
				continue
			}
			if err := s.remove(dir, entry.id); err != nil {
				return err
			}
			count--
			if dir == "audit" {
				s.dropped++
			}
			total -= entry.size
		}
	}
	if s.dropped > 0 {
		if err := s.write("audit", retentionID, Audit{RequestID: retentionID, At: now, Outcome: "retention", Count: s.dropped}); err != nil {
			return err
		}
	}
	return nil
}
