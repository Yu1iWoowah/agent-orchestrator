package appdiagnostics

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

type diskStore struct {
	writeContent func(*os.File, []byte) (int, error)
	root         *os.Root
	dropped      uint64
	skipped      uint64
	invalid      map[string]bool
	auditTimes   map[string]time.Time
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
	s := &diskStore{root: root, writeContent: (*os.File).Write, invalid: map[string]bool{}, auditTimes: map[string]time.Time{}}
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
	f, err := createPrivateFile(s.root, tmp)
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

// removeEntry pins and verifies the managed directory before unlinking one
// basename. Root.Remove does not follow the final symlink; it never recurses.
func (s *diskStore) removeEntry(dir, name string) error {
	if (dir != "audit" && dir != "snapshots") || filepath.IsAbs(name) || filepath.Base(name) != name || name == "." || name == ".." {
		return errors.New("invalid diagnostic cleanup entry")
	}
	before, err := s.root.Lstat(dir)
	if err != nil {
		return err
	}
	if err := privateDirectoryInfo(before); err != nil {
		return err
	}
	root, err := s.root.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	file, err := root.Open(".")
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	if err := secureOpenedFile(file, true); err != nil {
		return err
	}
	opened, err := file.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(before, opened) {
		return errors.New("diagnostic cleanup directory changed")
	}
	if err := root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
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
	rows, err := documents[Record](s, "snapshots", validSnapshotRecord)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if row.value.GrantID == id {
			if err := s.remove("snapshots", row.id); err != nil {
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
	s.auditTimes[row.RequestID] = row.At
	// The index is built at startup/Sweep. Appending an audit never reads
	// snapshots or decodes the retained audit history under the service mutex.
	for len(s.auditTimes) > 9999 {
		var oldest string
		for id, at := range s.auditTimes {
			if oldest == "" || at.Before(s.auditTimes[oldest]) {
				oldest = id
			}
		}
		if err := s.remove("audit", oldest); err != nil {
			return err
		}
		delete(s.auditTimes, oldest)
		s.dropped++
	}
	return s.retentionSummary(row.At)
}

type document[T any] struct {
	id    string
	value T
	size  int64
}

// Invalid audit documents are skipped; invalid snapshot files are unlinked.
// No file name or content enters the audit; one bounded summary records the reason.
func documents[T any](s *diskStore, dir string, validate func(string, T) bool) ([]document[T], error) {
	entries, err := s.entries(dir)
	if err != nil {
		return nil, err
	}
	out := make([]document[T], 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		key := name
		valid := !entry.IsDir() && filepath.Ext(name) == ".json"
		if valid {
			key = name[:len(name)-5]
			_, err = uuid.Parse(key)
			valid = err == nil
		}
		var value T
		if valid {
			valid = s.read(dir, key, &value) == nil
		}
		if valid && validate != nil {
			valid = validate(key, value)
		}
		if !valid {
			identity := dir + "/" + name
			if !s.invalid[identity] {
				s.invalid[identity] = true
				s.skipped++
			}
			if dir == "snapshots" && !entry.IsDir() {
				if err := s.removeEntry(dir, name); err != nil {
					return nil, err
				}
			}
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		out = append(out, document[T]{key, value, info.Size()})
	}
	return out, nil
}

// Only a validated envelope can establish ownership of a retained snapshot.
func validSnapshotRecord(id string, row Record) bool {
	if row.ID != id || row.CreatedAt.IsZero() {
		return false
	}
	for _, value := range []string{row.GrantID, row.RequestID} {
		if parsed, err := uuid.Parse(value); err != nil || parsed == uuid.Nil {
			return false
		}
	}
	snapshot := row.Snapshot
	return snapshot.SchemaVersion == 1 && snapshot.InstanceID != "" && snapshot.WindowID != "" && snapshot.Generation > 0 && !snapshot.CapturedAt.IsZero() && snapshot.Completeness == "partial"
}

// reset never revives a credential or payload from another daemon epoch.
const retentionID = "00000000-0000-0000-0000-000000000000"

func (s *diskStore) reset(now time.Time) error {
	var summary Audit
	if err := s.read("audit", retentionID, &summary); err == nil {
		s.dropped = summary.Count
		s.skipped = summary.SkippedCount
	}
	for _, dir := range []string{"grants", "snapshots"} {
		entries, err := s.entries(dir)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.IsDir() {
				s.invalid[dir+"/"+entry.Name()] = true
				s.skipped++
				continue
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
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) != ".tmp" {
			continue
		}
		stem := strings.TrimSuffix(name, ".tmp")
		if len(stem) != 36 {
			continue
		}
		if _, err := uuid.Parse(stem); err != nil {
			continue
		}
		if err := s.removeEntry("audit", name); err != nil {
			return err
		}
	}
	rows, err := documents[Audit](s, "audit", nil)
	if err != nil {
		return err
	}
	for _, doc := range rows {
		row := doc.value
		if row.Outcome == "accepted" {
			row.At = now
			row.Outcome = "interrupted"
			if err := s.write("audit", doc.id, row); err != nil {
				return err
			}
		}
	}
	return s.prune(now)
}
func (s *diskStore) prune(now time.Time) error {
	rows, err := documents[Record](s, "snapshots", validSnapshotRecord)
	if err != nil {
		return err
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].value.CreatedAt.Before(rows[j].value.CreatedAt) })
	var total int64
	for _, row := range rows {
		total += row.size
	}
	count := len(rows)
	for _, row := range rows {
		if now.Before(row.value.CreatedAt.Add(24*time.Hour)) && count <= 20 && total <= 20<<20 {
			continue
		}
		if err := s.remove("snapshots", row.id); err != nil {
			return err
		}
		count--
		total -= row.size
	}
	audits, err := documents[Audit](s, "audit", nil)
	if err != nil {
		return err
	}
	sort.Slice(audits, func(i, j int) bool { return audits[i].value.At.Before(audits[j].value.At) })
	s.auditTimes = make(map[string]time.Time, len(audits))
	for _, row := range audits {
		if row.id != retentionID {
			s.auditTimes[row.id] = row.value.At
		}
	}
	for _, row := range audits {
		if row.id == retentionID {
			continue
		}
		if now.Before(row.value.At.Add(7*24*time.Hour)) && len(s.auditTimes) <= 9999 {
			continue
		}
		if err := s.remove("audit", row.id); err != nil {
			return err
		}
		delete(s.auditTimes, row.id)
		s.dropped++
	}
	return s.retentionSummary(now)
}
func (s *diskStore) retentionSummary(now time.Time) error {
	if s.dropped == 0 && s.skipped == 0 {
		return nil
	}
	row := Audit{RequestID: retentionID, At: now, Outcome: "retention", Count: s.dropped, SkippedCount: s.skipped}
	if s.skipped > 0 {
		row.ReasonCode = "APP_DIAGNOSTICS_DOCUMENT_INVALID"
	}
	return s.write("audit", retentionID, row)
}
