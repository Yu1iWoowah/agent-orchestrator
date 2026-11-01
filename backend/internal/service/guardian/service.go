package guardian

import (
	"bufio"
	"context"
	"errors"
	"io"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	usagesvc "github.com/aoagents/agent-orchestrator/backend/internal/service/usage"
)

// Config bounds collection and resolves registered provider roots.
type Config struct {
	// Roots resolves only an already registered provider account's history roots.
	Roots           func(context.Context, domain.GuardianOwner) ([]string, error)
	Clock           func() time.Time
	DiscoveryBudget int
	RecordBudget    int
}

// Service is single-flight and bounded. It observes after the approval path;
// read/parse failures never retry or affect a model's approval decision.
type Service struct {
	store       ports.GuardianStore
	cfg         Config
	mu          sync.Mutex
	lastCleanup time.Time
	discovery   map[string]*headerDiscovery
	checks      map[checkpointKey]checkpointProgress
}

// New constructs a metadata-only collector; it never starts a provider.
func New(store ports.GuardianStore, cfg Config) *Service {
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	if cfg.DiscoveryBudget <= 0 {
		cfg.DiscoveryBudget = 4096
	}
	if cfg.RecordBudget <= 0 {
		cfg.RecordBudget = 512
	}
	return &Service{store: store, cfg: cfg, discovery: map[string]*headerDiscovery{}, checks: map[checkpointKey]checkpointProgress{}}
}

// Collect cleans expired metadata and reads bounded new source sections.
func (s *Service) Collect(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.cfg.Clock().UTC()
	// Hourly checks give a strict <24h deletion lag even at clock boundaries.
	if s.lastCleanup.IsZero() || now.Sub(s.lastCleanup) >= time.Hour {
		if err := s.store.CleanupGuardian(ctx, now); err != nil {
			return err
		}
		s.lastCleanup = now
	}
	owners, err := s.store.GuardianOwners(ctx, now)
	if err != nil {
		return err
	}
	if len(owners) == 0 {
		clear(s.discovery)
		clear(s.checks)
		return nil
	}
	activeEpochs := make(map[int64]bool, len(owners))
	for _, owner := range owners {
		activeEpochs[owner.EpochID] = true
	}
	for key := range s.checks {
		if !activeEpochs[key.epoch] {
			delete(s.checks, key)
		}
	}
	// A native root claimed by multiple durable AO owners is ambiguous even if
	// its files happen to live under the same account's shared history.
	rootsClaimed := map[string]int{}
	for _, o := range owners {
		rootsClaimed[o.NativeRootID]++
	}
	type group struct {
		roots  []string
		owners []domain.GuardianOwner
	}
	groups := map[string]*group{}
	for _, o := range owners {
		if err := ctx.Err(); err != nil {
			return err
		}
		if o.OwnershipConflict || rootsClaimed[o.NativeRootID] != 1 {
			if err := s.anomaly(ctx, o, domain.GuardianSourceState{}, domain.GuardianOwnerConflict, now); err != nil {
				return err
			}
			continue
		}
		if s.cfg.Roots == nil {
			clear(s.checks)
			return errors.New("guardian roots unavailable")
		}
		roots, err := s.cfg.Roots(ctx, o)
		if err != nil || len(roots) == 0 {
			for key := range s.checks {
				if key.epoch == o.EpochID {
					delete(s.checks, key)
				}
			}
			if err := s.anomaly(ctx, o, domain.GuardianSourceState{}, domain.GuardianSourceMissing, now); err != nil {
				return err
			}
			continue
		}
		roots = slices.Clone(roots)
		sort.Strings(roots)
		roots = slices.Compact(roots)
		key := strings.Join(roots, "\x00")
		if groups[key] == nil {
			groups[key] = &group{roots: roots}
		}
		groups[key].owners = append(groups[key].owners, o)
	}
	for key := range s.discovery {
		if groups[key] == nil {
			delete(s.discovery, key)
		}
	}
	for key, g := range groups {
		if s.discovery[key] == nil {
			s.discovery[key] = &headerDiscovery{}
		}
		if err := s.collectGroup(ctx, g.roots, g.owners, rootsClaimed, s.discovery[key], now); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) collectGroup(ctx context.Context, roots []string, owners []domain.GuardianOwner, claims map[string]int, discovery *headerDiscovery, now time.Time) error {
	cursors := map[int64][]domain.GuardianCursor{}
	required := make([]string, 0, len(owners))
	for _, o := range owners {
		rows, err := s.store.GuardianCursors(ctx, o.EpochID)
		if err != nil {
			return err
		}
		cursors[o.EpochID] = rows
		required = append(required, o.NativeRootID)
		for _, c := range rows {
			required = append(required, c.ThreadID)
		}
	}
	for key := range s.checks {
		rows, active := cursors[key.epoch]
		if !active {
			continue
		}
		found := false
		for _, c := range rows {
			if c.ThreadID == key.thread {
				found = true
				break
			}
		}
		if !found {
			delete(s.checks, key)
		}
	}
	result := discovery.scan(ctx, roots, s.cfg.DiscoveryBudget, required)
	ids := make([]string, 0, len(result.index))
	for id, a := range result.index {
		if a.meta.Guardian {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, o := range owners {
		inventory := map[domain.GuardianCode]int64{}
		if result.gap {
			inventory[domain.GuardianCoverageGap] = 1
		}
		if err := s.observe(ctx, o, domain.GuardianSourceState{}, inventory, now); err != nil {
			return err
		}
		known := map[string]bool{}
		for _, c := range cursors[o.EpochID] {
			known[c.ThreadID] = true
			if !result.gap {
				if _, ok := result.index[c.ThreadID]; !ok {
					if err := s.anomaly(ctx, o, c.SourceState(), domain.GuardianSourceMissing, now); err != nil {
						return err
					}
				}
			}
		}
		for _, id := range ids {
			a := result.index[id]
			belongs, code := ownerCode(a.meta, o.NativeRootID, result.index, result.conflicts, claims, known)
			if !belongs {
				continue
			}
			if code != "" {
				if err := s.anomaly(ctx, o, a.state, code, now); err != nil {
					return err
				}
				continue
			}
			if !supportedVersion(a.meta.Version) {
				if err := s.anomaly(ctx, o, a.state, domain.GuardianUnsupportedVersion, now); err != nil {
					return err
				}
				continue
			}
			if err := s.ingest(ctx, o, a, now); err != nil {
				if errors.Is(err, domain.ErrGuardianCursorConflict) || errors.Is(err, domain.ErrGuardianDisabled) {
					continue
				}
				return err
			}
		}
	}
	return nil
}

// Resolve the endpoint before assigning an anomaly to an AO owner. Complete
// foreign roots and broken chains are not evidence against every local owner.
func ownerCode(h header, root string, index map[string]artifact, conflicts map[string]bool, claims map[string]int, known map[string]bool) (bool, domain.GuardianCode) {
	seen := map[string]bool{}
	ambiguous := false
	crossedClaim := false
	wasOwned := known[h.ID]
	for range 32 {
		if seen[h.ID] {
			if wasOwned {
				return true, domain.GuardianOwnerConflict
			}
			return false, ""
		}
		seen[h.ID] = true
		ambiguous = ambiguous || conflicts[h.ID]
		if h.ID == root {
			if h.Parent != "" || ambiguous || crossedClaim {
				return true, domain.GuardianOwnerConflict
			}
			return true, ""
		}
		if claims[h.ID] > 0 {
			crossedClaim = true
		}
		if h.Parent == "" {
			if wasOwned && claims[h.ID] == 0 {
				return true, domain.GuardianOwnerUnresolved
			}
			return false, ""
		}
		a, ok := index[h.Parent]
		if !ok {
			if wasOwned {
				return true, domain.GuardianOwnerUnresolved
			}
			return false, ""
		}
		h = a.meta
	}
	if wasOwned {
		return true, domain.GuardianCoverageGap
	}
	return false, ""
}

func (s *Service) anomaly(ctx context.Context, o domain.GuardianOwner, state domain.GuardianSourceState, code domain.GuardianCode, now time.Time) error {
	return s.observe(ctx, o, state, map[domain.GuardianCode]int64{code: 1}, now)
}

func (s *Service) observe(ctx context.Context, owner domain.GuardianOwner, source domain.GuardianSourceState, codes map[domain.GuardianCode]int64, now time.Time) error {
	err := s.store.ObserveGuardianAnomalies(ctx, owner, source, codes, now)
	if errors.Is(err, domain.ErrGuardianDisabled) {
		return nil
	}
	return err
}

// Rev2 has no fixed version list. These observed versions share the same
// metadata whitelist fixtures; future versions remain explicitly unsupported.
func supportedVersion(version string) bool {
	switch version {
	case "0.146.0", "0.164.0", "0.165.0", "0.166.0":
		return true
	default:
		return false
	}
}

func (s *Service) ingest(ctx context.Context, o domain.GuardianOwner, a artifact, now time.Time) error {
	cur, err := s.store.GuardianCursor(ctx, o.EpochID, a.meta.ID)
	if err != nil {
		return err
	}
	f, err := openFile(a.root, a.path)
	if err != nil {
		return s.anomaly(ctx, o, a.state, domain.GuardianSourceMissing, now)
	}
	defer func() { _ = f.Close() }()
	identity, err := usagesvc.SourceIdentityFromFile(f)
	if err != nil {
		return s.anomaly(ctx, o, a.state, domain.GuardianSourceMissing, now)
	}
	// A kernel inode may be reused after deletion. The bounded provider header
	// timestamp distinguishes that new source without retaining its native ID.
	identity += "@" + a.meta.CreatedAt.Format(time.RFC3339Nano)
	info, err := f.Stat()
	if err != nil {
		return s.anomaly(ctx, o, a.state, domain.GuardianSourceMissing, now)
	}
	b := domain.GuardianBatch{Owner: o, Thread: domain.GuardianThread{ProviderAt: a.meta.CreatedAt, ThreadID: a.meta.ID, ParentID: a.meta.Parent, CLIVersion: a.meta.Version}, Expected: cur, Cursor: cur, Anomalies: map[domain.GuardianCode]int64{}}
	position, err := s.store.GuardianPosition(ctx, o.EpochID, identity)
	if err != nil {
		return err
	}
	b.ExpectedPosition = position
	if cur.FileIdentity == "" && position.FileIdentity != "" {
		b.Cursor = position
	}
	b.Cursor.ThreadID = a.meta.ID

	sourceChanged := b.Cursor.FileIdentity != identity || b.Cursor.SourceSize != info.Size() || !b.Cursor.SourceModifiedAt.Equal(info.ModTime())
	growth := b.Cursor.FileIdentity == identity && info.Size() > b.Cursor.SourceSize
	readEnd := max(b.Cursor.Offset, b.Cursor.ReplayUntil)
	b.Cursor.UpdatedAt = now
	if b.Cursor.FileIdentity != "" && b.Cursor.FileIdentity != identity {
		b.Cursor.Generation++
		b.Cursor.Offset = 0
		b.Cursor.ReadStartOffset = 0
		b.Cursor.ReplayUntil = 0
		b.Cursor.ReplayOffset = 0
		b.Cursor.ReplayDiscarding = false
		b.Cursor.ReplayAgain = false
		b.Cursor.Discarding = false
		b.Anomalies[domain.GuardianSourceRotated]++
	}
	if info.Size() < b.Cursor.Offset || b.Cursor.FileIdentity == identity && info.Size() < b.Cursor.SourceSize {
		b.Cursor.Generation++
		b.Cursor.Offset = 0
		b.Cursor.ReadStartOffset = 0
		b.Cursor.ReplayUntil = min(readEnd, info.Size())
		b.Cursor.ReplayOffset = 0
		b.Cursor.ReplayDiscarding = false
		b.Cursor.ReplayAgain = false
		b.Cursor.Discarding = false
		b.Anomalies[domain.GuardianSourceTruncated]++
	}
	b.Cursor.FileIdentity = identity
	// Revalidate the header from the exact descriptor, closing discovery/open races.
	line, err := bufio.NewReaderSize(io.LimitReader(f, maxLine+1), 4096).ReadBytes('\n')
	if err != nil || len(line) > maxLine {
		return s.anomaly(ctx, o, a.state, domain.GuardianInvalidMetadata, now)
	}
	actual, ok := parseHeader(line)
	if !ok || actual != a.meta {
		return s.anomaly(ctx, o, a.state, domain.GuardianSourceConflict, now)
	}
	if b.Cursor.ReadStartOffset == 0 {
		b.Cursor.ReadStartOffset = int64(len(line))
	}
	if b.Cursor.Offset > b.Cursor.ReadStartOffset {
		if b.Cursor.ReplayUntil > b.Cursor.Offset && b.Cursor.ReplayOffset == 0 {
			if sourceChanged && !growth {
				b.Cursor.Generation++
				b.Cursor.ReplayUntil = min(readEnd, info.Size())
				b.Cursor.Offset = b.Cursor.ReadStartOffset
				b.Cursor.Discarding = false
				b.Cursor.ReplayAgain = false
			} else if sourceChanged {
				b.Cursor.ReplayAgain = true
			}
		} else {
			rewritten, err := s.checkpoints(ctx, f, b.Cursor)
			if err != nil {
				return err
			}
			if rewritten || sourceChanged && !growth {
				b.Cursor.Generation++
				b.Cursor.ReplayUntil = b.Cursor.Offset
				b.Cursor.Offset = b.Cursor.ReadStartOffset
				b.Cursor.Discarding = false
				b.Cursor.ReplayOffset = 0
				b.Cursor.ReplayDiscarding = false
				b.Cursor.ReplayAgain = false
				if rewritten {
					b.Anomalies[domain.GuardianSourceTruncated]++
				}
			} else if sourceChanged && growth {
				if b.Cursor.ReplayOffset > 0 {
					b.Cursor.ReplayAgain = true
				} else {
					b.Cursor.ReplayOffset = b.Cursor.ReadStartOffset
					b.Cursor.ReplayUntil = b.Cursor.Offset
				}
			}
		}
	}
	b.Cursor.SourceSize = info.Size()
	b.Cursor.SourceModifiedAt = info.ModTime().UTC()
	if b.Cursor.Offset == 0 {
		b.Cursor.Offset = int64(len(line))
		b.Cursor.ReadStartOffset = int64(len(line))
		// Preserve the original enable fence when an older source is replaced
		// or shrunk. A fresh provider header can prove a newly readable source.
		if !o.AcceptsProviderTime(a.meta.CreatedAt, now) && (b.Expected.FileIdentity == "" || b.Expected.ReadStartOffset > int64(len(line))) {
			b.Cursor.Offset = info.Size()
			b.Cursor.ReadStartOffset = info.Size()
			b.Cursor.ReplayUntil = 0
			b.Cursor.ReplayOffset = 0
			b.Cursor.ReplayDiscarding = false
			b.Cursor.ReplayAgain = false
			b.Anomalies[domain.GuardianCoverageGap]++
		}
	}
	if !sourceChanged && b.Cursor.Offset == info.Size() && b.Cursor.ReplayUntil == 0 && !b.Cursor.Discarding && len(b.Anomalies) == 0 {
		return s.observe(ctx, o, b.Cursor.SourceState(), nil, now)
	}
	revalidationEnd := b.Cursor.ReplayUntil
	if b.Cursor.ReplayOffset > 0 {
		revalidationEnd = 0
	}
	partial, err := startsInsideLine(ctx, f, b.Cursor.Offset)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return s.anomaly(ctx, o, a.state, domain.GuardianSourceMissing, now)
	}
	b.Cursor.Discarding = partial
	if _, err := f.Seek(b.Cursor.Offset, io.SeekStart); err != nil {
		return s.anomaly(ctx, o, a.state, domain.GuardianSourceMissing, now)
	}
	reader := bufio.NewReaderSize(f, maxLine+1)
	startOffset := b.Cursor.Offset
	recordsRead, bytesRead := 0, int64(0)
	for n := 0; n < s.cfg.RecordBudget; n++ {
		if b.Cursor.Offset-startOffset >= 4*maxLine {
			if b.Cursor.Offset >= revalidationEnd && b.Cursor.Offset < info.Size() {
				b.Anomalies[domain.GuardianCoverageGap]++
			}
			break
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		offset := b.Cursor.Offset
		line, err := reader.ReadSlice('\n')
		bytesRead += int64(len(line))
		if len(line) > 0 {
			recordsRead++
		}
		if errors.Is(err, io.EOF) {
			if len(line) > 0 && b.Cursor.Discarding {
				b.Cursor.Offset += int64(len(line))
			}
			if len(line) > 0 {
				b.Anomalies[domain.GuardianPartialLine]++
			}
			break
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			b.Cursor.Offset += int64(len(line))
			b.Cursor.Discarding = true
			b.Anomalies[domain.GuardianInvalidMetadata]++
			continue
		}
		if err != nil {
			return s.anomaly(ctx, o, a.state, domain.GuardianSourceMissing, now)
		}
		b.Cursor.Offset += int64(len(line))
		if b.Cursor.Discarding {
			b.Cursor.Discarding = false
		} else {
			appendRecord(&b, line, offset)
		}
		if n == s.cfg.RecordBudget-1 && b.Cursor.Offset < info.Size() && b.Cursor.Offset >= revalidationEnd {
			b.Anomalies[domain.GuardianCoverageGap]++
		}
	}
	if revalidationEnd > 0 {
		if err := s.revalidationGap(ctx, &b, revalidationEnd); err != nil {
			return err
		}
		if b.Cursor.Offset >= revalidationEnd {
			finishRevalidation(&b.Cursor)
		}
	}
	if b.Cursor.ReplayOffset > 0 {
		if err := s.verifyPrefix(ctx, f, &b, s.cfg.RecordBudget-recordsRead, 4*maxLine-bytesRead); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return s.anomaly(ctx, o, a.state, domain.GuardianSourceMissing, now)
		}
	}
	// Detect an in-place truncation while reading before committing its cursor.
	if after, err := f.Stat(); err != nil || after.Size() < b.Cursor.Offset {
		return s.anomaly(ctx, o, a.state, domain.GuardianSourceTruncated, now)
	}
	if len(b.Requests) == 0 && len(b.Models) == 0 && sameReadPosition(b.Cursor, b.Expected) {
		return s.observe(ctx, o, b.Cursor.SourceState(), b.Anomalies, now)
	}
	return s.store.ApplyGuardianBatch(ctx, b)
}

func sameTokens(a, b domain.GuardianTokens) bool {
	equal := func(x, y *int64) bool { return x == nil && y == nil || x != nil && y != nil && *x == *y }
	return equal(a.Input, b.Input) && equal(a.Cached, b.Cached) && equal(a.CacheWrite, b.CacheWrite) && equal(a.Output, b.Output) && equal(a.Reasoning, b.Reasoning) && equal(a.Total, b.Total)
}

func sameReadPosition(a, b domain.GuardianCursor) bool {
	return a.FileIdentity == b.FileIdentity && a.Offset == b.Offset && a.Generation == b.Generation && a.ReadStartOffset == b.ReadStartOffset && a.ReplayUntil == b.ReplayUntil && a.ReplayOffset == b.ReplayOffset && a.ReplayDiscarding == b.ReplayDiscarding && a.ReplayAgain == b.ReplayAgain && a.Discarding == b.Discarding && a.SourceSize == b.SourceSize && a.SourceModifiedAt.Equal(b.SourceModifiedAt)
}
