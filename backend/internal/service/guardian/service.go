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
	return &Service{store: store, cfg: cfg, discovery: map[string]*headerDiscovery{}}
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
		return nil
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
			if err := s.anomaly(ctx, o, domain.GuardianOwnerConflict, now); err != nil {
				return err
			}
			continue
		}
		if s.cfg.Roots == nil {
			return errors.New("guardian roots unavailable")
		}
		roots, err := s.cfg.Roots(ctx, o)
		if err != nil {
			if err := s.anomaly(ctx, o, domain.GuardianSourceMissing, now); err != nil {
				return err
			}
			continue
		}
		if len(roots) == 0 {
			if err := s.anomaly(ctx, o, domain.GuardianSourceMissing, now); err != nil {
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
	result := discovery.scan(ctx, roots, s.cfg.DiscoveryBudget, required, now)
	ids := make([]string, 0, len(result.index))
	for id, a := range result.index {
		if a.meta.Guardian {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, o := range owners {
		if result.gap {
			if err := s.anomaly(ctx, o, domain.GuardianCoverageGap, now); err != nil {
				return err
			}
		}
		known := map[string]bool{}
		for _, c := range cursors[o.EpochID] {
			known[c.ThreadID] = true
			if !result.gap {
				if _, ok := result.index[c.ThreadID]; !ok {
					if err := s.anomaly(ctx, o, domain.GuardianSourceMissing, now); err != nil {
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
				if err := s.anomaly(ctx, o, code, now); err != nil {
					return err
				}
				continue
			}
			if a.meta.Version != "0.146.0" && a.meta.Version != "0.166.0" {
				if err := s.anomaly(ctx, o, domain.GuardianUnsupportedVersion, now); err != nil {
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
			return false, ""
		}
		a, ok := index[h.Parent]
		if !ok {
			return false, ""
		}
		h = a.meta
	}
	if wasOwned {
		return true, domain.GuardianCoverageGap
	}
	return false, ""
}

func (s *Service) anomaly(ctx context.Context, o domain.GuardianOwner, code domain.GuardianCode, now time.Time) error {
	return s.store.RecordGuardianAnomaly(ctx, o.SessionID, code, now)
}

func (s *Service) ingest(ctx context.Context, o domain.GuardianOwner, a artifact, now time.Time) error {
	cur, err := s.store.GuardianCursor(ctx, o.EpochID, a.meta.ID)
	if err != nil {
		return err
	}
	f, err := openFile(a.root, a.path)
	if err != nil {
		return s.anomaly(ctx, o, domain.GuardianSourceMissing, now)
	}
	defer func() { _ = f.Close() }()
	identity, err := usagesvc.SourceIdentityFromFile(f)
	if err != nil {
		return s.anomaly(ctx, o, domain.GuardianSourceMissing, now)
	}
	// A kernel inode may be reused after deletion. The bounded provider header
	// timestamp distinguishes that new source without retaining its native ID.
	identity += "@" + a.meta.CreatedAt.Format(time.RFC3339Nano)
	info, err := f.Stat()
	if err != nil {
		return s.anomaly(ctx, o, domain.GuardianSourceMissing, now)
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

	b.Cursor.UpdatedAt = now
	if b.Cursor.FileIdentity != "" && b.Cursor.FileIdentity != identity {
		b.Cursor.Generation++
		b.Cursor.Offset = 0
		b.Cursor.Discarding = false
		b.Anomalies[domain.GuardianSourceRotated]++
	}
	if info.Size() < b.Cursor.Offset {
		b.Cursor.Generation++
		b.Cursor.Offset = 0
		b.Cursor.Discarding = false
		b.Anomalies[domain.GuardianSourceTruncated]++
	}
	b.Cursor.FileIdentity = identity
	// Revalidate the header from the exact descriptor, closing discovery/open races.
	reader := bufio.NewReaderSize(f, maxLine+1)
	line, err := reader.ReadSlice('\n')
	if err != nil || len(line) > maxLine {
		return s.anomaly(ctx, o, domain.GuardianInvalidMetadata, now)
	}
	actual, ok := parseHeader(line)
	if !ok || actual != a.meta {
		return s.anomaly(ctx, o, domain.GuardianSourceConflict, now)
	}
	// Size alone cannot detect truncate-and-regrow on the same inode. When the
	// resuming, verify a retained immutable request at its recorded offset.
	// Missing/expired evidence is an explicit gap, never proof of continuity.
	if b.Cursor.Offset > int64(len(line)) {
		checkpoint, found, err := s.store.GuardianCheckpoint(ctx, b.Cursor)
		if err != nil {
			return err
		}
		if found && !matchesCheckpoint(f, checkpoint, b) {
			b.Cursor.Generation++
			b.Cursor.Offset = int64(len(line))
			b.Cursor.Discarding = false
			b.Anomalies[domain.GuardianSourceTruncated]++
		} else if !found && (b.Cursor.SourceSize != info.Size() || !b.Cursor.SourceModifiedAt.Equal(info.ModTime())) {
			b.Anomalies[domain.GuardianCoverageGap]++
			// An unproven append must not replay bytes before the enable boundary.
			if info.Size() <= b.Cursor.SourceSize {
				b.Cursor.Generation++
				b.Cursor.Offset = int64(len(line))
				b.Cursor.Discarding = false
			}
		}
	}
	b.Cursor.SourceSize = info.Size()
	b.Cursor.SourceModifiedAt = info.ModTime().UTC()
	if b.Expected.FileIdentity == "" && b.Cursor.Offset == 0 {
		// Existing bytes are an enable boundary, never a historical backfill. A
		// trailing partial line is consumed only after it becomes a full record.
		// Records are independently timestamp-gated even on new/rotated sources.
		b.Cursor.Offset = int64(len(line))
		if !o.AcceptsProviderTime(a.meta.CreatedAt, now) {
			b.Cursor.Offset = info.Size()
			b.Anomalies[domain.GuardianCoverageGap]++
		}
	}
	if b.Cursor.Offset == 0 {
		b.Cursor.Offset = int64(len(line))
	}
	if _, err := f.Seek(b.Cursor.Offset, io.SeekStart); err != nil {
		return s.anomaly(ctx, o, domain.GuardianSourceMissing, now)
	}
	reader.Reset(f)
	startOffset := b.Cursor.Offset
	for n := 0; n < s.cfg.RecordBudget; n++ {
		if b.Cursor.Offset-startOffset >= 4*maxLine {
			b.Anomalies[domain.GuardianCoverageGap]++
			break
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		offset := b.Cursor.Offset
		line, err := reader.ReadSlice('\n')
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
			return s.anomaly(ctx, o, domain.GuardianSourceMissing, now)
		}
		b.Cursor.Offset += int64(len(line))
		if b.Cursor.Discarding {
			b.Cursor.Discarding = false
		} else {
			appendRecord(&b, line, offset)
		}
		if n == s.cfg.RecordBudget-1 && b.Cursor.Offset < info.Size() {
			b.Anomalies[domain.GuardianCoverageGap]++
		}
	}
	// Detect an in-place truncation while reading before committing its cursor.
	if after, err := f.Stat(); err != nil || after.Size() < b.Cursor.Offset {
		return s.anomaly(ctx, o, domain.GuardianSourceTruncated, now)
	}
	return s.store.ApplyGuardianBatch(ctx, b)
}

// matchesCheckpoint reads at most one bounded line and compares only the typed
// request whitelist. No provider bytes, content hashes or paths are persisted.
func matchesCheckpoint(f io.ReadSeeker, expected domain.GuardianRequest, batch domain.GuardianBatch) bool {
	if _, err := f.Seek(expected.SourceOffset, io.SeekStart); err != nil {
		return false
	}
	line, err := bufio.NewReaderSize(io.LimitReader(f, maxLine+1), 4096).ReadBytes('\n')
	if err != nil || len(line) > maxLine {
		return false
	}
	probe := domain.GuardianBatch{Owner: batch.Owner, Thread: batch.Thread, Cursor: batch.Cursor, Anomalies: map[domain.GuardianCode]int64{}}
	// Historical retained evidence remains useful across enable boundaries.
	probe.Owner.EnabledAt = time.Time{}
	probe.Owner.AccountBoundAt = time.Time{}
	probe.Owner.CleanedBefore = time.Time{}
	appendRecord(&probe, line, expected.SourceOffset)
	if len(probe.Requests) != 1 {
		return false
	}
	got := probe.Requests[0]
	return got.ThreadID == expected.ThreadID && got.TurnID == expected.TurnID && got.ResponseID == expected.ResponseID && got.RootTurnID == expected.RootTurnID && got.ProviderAt.Equal(expected.ProviderAt) && sameTokens(got.Tokens, expected.Tokens)
}
func sameTokens(a, b domain.GuardianTokens) bool {
	equal := func(x, y *int64) bool { return x == nil && y == nil || x != nil && y != nil && *x == *y }
	return equal(a.Input, b.Input) && equal(a.Cached, b.Cached) && equal(a.CacheWrite, b.CacheWrite) && equal(a.Output, b.Output) && equal(a.Reasoning, b.Reasoning) && equal(a.Total, b.Total)
}
