package rca

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/components/hcg/policy"
	rcapolicy "github.com/aoagents/agent-orchestrator/components/hcg/rca"
)

var errStorage = errors.New("RCA storage unavailable")

type storedSession struct {
	ProjectID domain.ProjectID         `json:"projectId"`
	SessionID domain.SessionID         `json:"sessionId"`
	Manifest  policy.Manifest          `json:"manifest"`
	Recorded  *domain.RCARecordedState `json:"recorded"`
}
type storedState struct {
	Version  int                      `json:"version"`
	Host     rcapolicy.HostState      `json:"host"`
	Sessions map[string]storedSession `json:"sessions"`
}

// DiskStore serializes every RCA metadata and numeric write across projects.
// The old document plus the complete replacement staging document count toward
// both quotas. There is no WAL, raw cache, journal, log or retry spool.
type DiskStore struct {
	mu                  sync.Mutex
	reasonMu            sync.RWMutex
	staged              fs.FileInfo
	base, dir           *os.Root
	path                string
	baseInfo, dirInfo   fs.FileInfo
	volumeID, reason    string
	maxHost, maxSession uint64
	space               func(*os.Root) (volumeSpace, error)
	write               func(*os.File, []byte) (int, error)
	flush               func(*os.File) error
	replace             func(*os.Root, string, string) error
	remove              func(*os.Root, string) error
	now                 func() time.Time
}

// NewDiskStore opens the owned numeric projection boundary.
func NewDiskStore(root string) (*DiskStore, error) {
	if !filepath.IsAbs(root) {
		return nil, errStorage
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, errStorage
	}
	info, err := r.Stat(".")
	if err != nil {
		_ = r.Close()
		return nil, errStorage
	}
	s := &DiskStore{base: r, path: root, baseInfo: info, maxHost: hostBytes, maxSession: sessionBytes, space: currentVolume, now: time.Now,
		write: func(f *os.File, b []byte) (int, error) { return f.Write(b) }, flush: func(f *os.File) error { return f.Sync() },
		replace: func(r *os.Root, a, b string) error { return r.Rename(a, b) }, remove: func(r *os.Root, p string) error { return r.Remove(p) }}
	// Startup expiration uses the same writer admission; a failure leaves reads
	// of safe, unexpired data possible while latching new RCA writes off.
	_, _ = s.read(context.Background(), true)
	return s, nil
}

// Close releases owned directory handles.
func (s *DiskStore) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dir != nil {
		_ = s.dir.Close()
	}
	_ = s.base.Close()
}

// Reason returns a fixed, latched RCA storage reason.
func (s *DiskStore) Reason() string { s.reasonMu.RLock(); defer s.reasonMu.RUnlock(); return s.reason }
func (s *DiskStore) fail(reason string) error {
	s.reasonMu.Lock()
	defer s.reasonMu.Unlock()
	if s.reason == "" {
		s.reason = reason
	}
	return errStorage
}
func keyString(k rcapolicy.Key) string { return k.ProjectID + "/" + k.SessionID }

func (s *DiskStore) directory() (bool, error) {
	info, err := os.Stat(s.path)
	if err != nil || !os.SameFile(s.baseInfo, info) {
		return false, s.fail("storage_identity_changed")
	}
	entry, err := s.base.Lstat("rca")
	if os.IsNotExist(err) {
		if s.dir != nil {
			return false, s.fail("storage_identity_changed")
		}
		return false, nil
	}
	if err != nil || !entry.IsDir() || entry.Mode()&os.ModeSymlink != 0 {
		return false, s.fail("storage_identity_changed")
	}
	if s.dir == nil {
		s.dir, err = s.base.OpenRoot("rca")
		if err != nil {
			return false, s.fail("storage_read_failed")
		}
		s.dirInfo, err = s.dir.Stat(".")
		if err != nil {
			return false, s.fail("storage_read_failed")
		}
	}
	if !os.SameFile(entry, s.dirInfo) {
		return false, s.fail("storage_identity_changed")
	}
	return true, nil
}

func (s *DiskStore) read(ctx context.Context, cleanup bool) (out storedState, readErr error) {
	out = storedState{Version: 1, Sessions: map[string]storedSession{}}
	// json.Decoder can populate nested rows before reporting an error. No
	// partially decoded or unvalidated state may cross this read boundary.
	defer func() {
		if readErr != nil {
			out = storedState{}
		}
	}()
	if ctx.Err() != nil {
		return out, errStorage
	}
	exists, err := s.directory()
	if err != nil || !exists {
		return out, err
	}
	info, err := s.dir.Lstat("state.json")
	if os.IsNotExist(err) {
		return out, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > int64(hostBytes) {
		return out, s.fail("storage_read_failed")
	}
	f, err := s.dir.Open("state.json")
	if err != nil {
		return out, s.fail("storage_read_failed")
	}
	defer func() { _ = f.Close() }()
	d := json.NewDecoder(io.LimitReader(f, int64(hostBytes)+1))
	d.DisallowUnknownFields()
	if d.Decode(&out) != nil || d.Decode(new(any)) != io.EOF || out.Version != 1 || out.Sessions == nil {
		return out, s.fail("storage_read_failed")
	}
	changed := false
	for key, row := range out.Sessions {
		if key != keyString(rcapolicy.Key{ProjectID: string(row.ProjectID), SessionID: string(row.SessionID)}) || !safeStoredSession(row) {
			return storedState{}, s.fail("storage_read_failed")
		}
		if cleanup && row.Recorded != nil && !s.now().Before(row.Recorded.ObservedAt.Add(7*24*time.Hour)) {
			row.Recorded = nil
			if row.Manifest.Revision == 0 {
				delete(out.Sessions, key)
			} else {
				out.Sessions[key] = row
			}
			changed = true
		}
	}
	if changed && cleanup && s.reason == "" {
		if err := s.save(ctx, out); err != nil {
			return out, err
		}
	}
	return out, nil
}

func (s *DiskStore) admission(peak uint64) error {
	if s.reason != "" {
		return errStorage
	}
	exists, err := s.directory()
	if err != nil {
		return err
	}
	root := s.base
	if exists {
		root = s.dir
	}
	v, err := s.space(root)
	if err != nil || v.Identity == "" || v.Total == 0 || v.Available > v.Total {
		return s.fail("storage_space_unknown")
	}
	if s.volumeID != "" && s.volumeID != v.Identity {
		return s.fail("storage_identity_changed")
	}
	s.volumeID = v.Identity
	if !spaceAllows(v, peak) {
		return s.fail("storage_low_space")
	}
	return nil
}

// Ready performs no write. A failure is latched until an explicit control
// operation invokes Resume and rechecks storage, policy and source authority.
func (s *DiskStore) Ready(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx.Err() != nil {
		return errStorage
	}
	return s.admission(storageMetadataBytes)
}

func (s *DiskStore) save(ctx context.Context, next storedState) error {
	if err := s.prepare(ctx, next); err != nil {
		return err
	}
	return s.finish(ctx, true)
}

// prepare writes only the existing atomic staging file. It cannot replace the
// durable projection until the shared consumer grants publication authority.
func (s *DiskStore) prepare(ctx context.Context, next storedState) (prepareErr error) {
	if s.reason != "" || ctx.Err() != nil {
		return errStorage
	}
	for key, row := range next.Sessions {
		if row.Recorded != nil && !s.now().Before(row.Recorded.ObservedAt.Add(7*24*time.Hour)) {
			row.Recorded = nil
			if row.Manifest.Revision == 0 {
				delete(next.Sessions, key)
			} else {
				next.Sessions[key] = row
			}
		}
	}
	data, err := json.Marshal(next)
	if err != nil {
		return s.fail("storage_write_failed")
	}
	old, err := s.read(ctx, false)
	if err != nil {
		return err
	}
	var oldBytes uint64
	if s.dir != nil {
		if info, err := s.dir.Stat("state.json"); err == nil {
			size := info.Size()
			if size < 0 || size > int64(hostBytes) {
				return s.fail("storage_read_failed")
			}
			oldBytes = uint64(size)
		} else if !os.IsNotExist(err) {
			return s.fail("storage_read_failed")
		}
	}
	if oldBytes+uint64(len(data))+storageMetadataBytes > s.maxHost {
		return s.fail("host_limit")
	}
	for key, row := range next.Sessions {
		b, err := json.Marshal(row)
		if err != nil {
			return s.fail("storage_write_failed")
		}
		before := []byte(nil)
		if prev, ok := old.Sessions[key]; ok {
			before, err = json.Marshal(prev)
			if err != nil {
				return s.fail("storage_write_failed")
			}
		}
		if uint64(len(before)+len(b))+uint64(2*len(key))+1024 > s.maxSession {
			return s.fail("session_limit")
		}
	}
	if err := s.admission(uint64(len(data)) + storageMetadataBytes); err != nil {
		return err
	}
	if s.dir == nil {
		if err := s.base.Mkdir("rca", 0o700); err != nil {
			return s.fail("storage_write_failed")
		}
		if _, err := s.directory(); err != nil {
			return err
		}
		if err := s.admission(uint64(len(data)) + storageMetadataBytes); err != nil {
			return err
		}
	}
	if _, err := s.dir.Lstat("state.next"); !os.IsNotExist(err) {
		return s.fail("storage_write_failed")
	}
	f, err := s.dir.OpenFile("state.next", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return s.fail("storage_write_failed")
	}
	prepared := false
	defer func() {
		_ = f.Close()
		if !prepared {
			if err := s.remove(s.dir, "state.next"); err != nil && !os.IsNotExist(err) {
				prepareErr = s.fail("storage_write_failed")
			}
		}
	}()
	written, err := s.write(f, data)
	if err != nil || written != len(data) {
		return s.fail("storage_write_failed")
	}
	if s.flush(f) != nil {
		return s.fail("storage_write_failed")
	}
	s.staged, err = f.Stat()
	if err != nil || f.Close() != nil {
		s.staged = nil
		return s.fail("storage_write_failed")
	}
	prepared = true
	return nil
}
func (s *DiskStore) finish(ctx context.Context, accepted bool) (finishErr error) {
	if s.staged == nil {
		return nil
	}
	staged := s.staged
	s.staged = nil
	defer func() {
		if err := s.remove(s.dir, "state.next"); err != nil && !os.IsNotExist(err) {
			finishErr = s.fail("storage_write_failed")
		}
	}()
	if !accepted || ctx.Err() != nil {
		return nil
	}
	info, err := s.dir.Lstat("state.next")
	if err != nil || !info.Mode().IsRegular() || !os.SameFile(staged, info) {
		return s.fail("storage_identity_changed")
	}
	if _, err := s.directory(); err != nil {
		return err
	}
	if s.replace(s.dir, "state.next", "state.json") != nil {
		return s.fail("storage_write_failed")
	}
	dir, err := s.dir.Open(".")
	if err != nil {
		return s.fail("storage_write_failed")
	}
	defer func() { _ = dir.Close() }()
	if s.flush(dir) != nil {
		return s.fail("storage_write_failed")
	}
	return nil
}

// Resume rechecks storage after an explicit recovery action.
func (s *DiskStore) Resume(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reasonMu.Lock()
	s.reason = ""
	s.reasonMu.Unlock()
	if err := s.admission(storageMetadataBytes); err != nil {
		return err
	}
	if s.dir != nil {
		if info, err := s.dir.Lstat("state.next"); err == nil {
			if !info.Mode().IsRegular() || s.remove(s.dir, "state.next") != nil {
				return s.fail("storage_write_failed")
			}
		} else if !os.IsNotExist(err) {
			return s.fail("storage_read_failed")
		}
	}
	_, err := s.read(ctx, true)
	return err
}

// Load reads one session RCA policy manifest.
func (s *DiskStore) Load(ctx context.Context, k rcapolicy.Key) (policy.Manifest, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.read(ctx, true)
	row, ok := state.Sessions[keyString(k)]
	return row.Manifest, ok && row.Manifest.Revision > 0, err
}

// Save admits an atomic RCA policy update.
func (s *DiskStore) Save(ctx context.Context, k rcapolicy.Key, m policy.Manifest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.read(ctx, false)
	if err != nil {
		return err
	}
	row := state.Sessions[keyString(k)]
	row.ProjectID, row.SessionID, row.Manifest = domain.ProjectID(k.ProjectID), domain.SessionID(k.SessionID), m
	if !safeStoredSession(row) {
		return s.fail("storage_write_failed")
	}
	state.Sessions[keyString(k)] = row
	return s.save(ctx, state)
}

// LoadHost reads the host RCA stop state.
func (s *DiskStore) LoadHost(ctx context.Context) (rcapolicy.HostState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.read(ctx, true)
	return state.Host, err
}

// SaveHost admits an atomic host RCA stop update.
func (s *DiskStore) SaveHost(ctx context.Context, h rcapolicy.HostState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.read(ctx, false)
	if err != nil {
		return err
	}
	state.Host = h
	return s.save(ctx, state)
}

// LoadSnapshot returns unexpired numeric data.
func (s *DiskStore) LoadSnapshot(ctx context.Context, project domain.ProjectID, id domain.SessionID) (domain.RCARecordedState, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.read(ctx, true)
	if err != nil {
		return domain.RCARecordedState{}, false, err
	}
	row := state.Sessions[keyString(rcapolicy.Key{ProjectID: string(project), SessionID: string(id)})]
	if row.Recorded == nil || !s.now().Before(row.Recorded.ObservedAt.Add(7*24*time.Hour)) {
		return domain.RCARecordedState{}, false, err
	}
	return *row.Recorded, true, err
}

// StageSnapshot prepares a candidate without changing state.json.
func (s *DiskStore) StageSnapshot(ctx context.Context, value domain.RCARecordedState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.read(ctx, false)
	if err != nil {
		return err
	}
	k := rcapolicy.Key{ProjectID: string(value.ProjectID), SessionID: string(value.SessionID)}
	row := state.Sessions[keyString(k)]
	row.ProjectID, row.SessionID, row.Recorded = value.ProjectID, value.SessionID, &value
	if !safeStoredSession(row) {
		return s.fail("storage_write_failed")
	}
	state.Sessions[keyString(k)] = row
	return s.prepare(ctx, state)
}
func (s *DiskStore) FinishSnapshot(ctx context.Context, accepted bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.finish(ctx, accepted)
}

// SaveSnapshot admits a replacement numeric projection.
func (s *DiskStore) SaveSnapshot(ctx context.Context, value domain.RCARecordedState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.read(ctx, false)
	if err != nil {
		return err
	}
	k := rcapolicy.Key{ProjectID: string(value.ProjectID), SessionID: string(value.SessionID)}
	row := state.Sessions[keyString(k)]
	row.ProjectID, row.SessionID, row.Recorded = value.ProjectID, value.SessionID, &value
	if !safeStoredSession(row) {
		return s.fail("storage_write_failed")
	}
	state.Sessions[keyString(k)] = row
	return s.save(ctx, state)
}

// Purge removes numeric scope while retaining policy overrides.
func (s *DiskStore) PurgeNumbers(ctx context.Context, key rcapolicy.Key) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.read(ctx, false)
	if err != nil {
		return err
	}
	k := keyString(key)
	row, ok := state.Sessions[k]
	if !ok {
		return nil
	}
	row.Recorded = nil
	state.Sessions[k] = row
	return s.save(ctx, state)
}
