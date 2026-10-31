package guardian

import (
	"bufio"
	"context"
	"errors"
	"io"
	"sort"
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
	return &Service{store: store, cfg: cfg}
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
		return nil
	}
	// A native root claimed by multiple durable AO owners is ambiguous even if
	// its files happen to live under the same account's shared history.
	rootsClaimed := map[string]int{}
	for _, o := range owners {
		rootsClaimed[o.NativeRootID]++
	}
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
		index, conflicts, gap := discover(ctx, roots, s.cfg.DiscoveryBudget)
		if gap {
			if err := s.anomaly(ctx, o, domain.GuardianCoverageGap, now); err != nil {
				return err
			}
		}
		if !gap {
			cursors, err := s.store.GuardianCursors(ctx, o.EpochID)
			if err != nil {
				return err
			}
			for _, c := range cursors {
				if _, ok := index[c.ThreadID]; !ok {
					if err := s.anomaly(ctx, o, domain.GuardianSourceMissing, now); err != nil {
						return err
					}
				}
			}
		}
		ids := make([]string, 0, len(index))
		for id, a := range index {
			if a.meta.Guardian {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
		for _, id := range ids {
			a := index[id]
			code := ownerCode(a.meta, o.NativeRootID, index, conflicts, rootsClaimed)
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

func ownerCode(h header, root string, index map[string]artifact, conflicts map[string]bool, claims map[string]int) domain.GuardianCode {
	seen := map[string]bool{}
	for depth := 0; depth < 32; depth++ {
		if seen[h.ID] || conflicts[h.ID] {
			return domain.GuardianOwnerConflict
		}
		seen[h.ID] = true
		if h.ID == root {
			if h.Parent != "" {
				return domain.GuardianOwnerConflict
			}
			return ""
		}
		if claims[h.ID] > 0 {
			return domain.GuardianOwnerConflict
		}
		a, ok := index[h.Parent]
		if !ok {
			return domain.GuardianOwnerUnresolved
		}
		h = a.meta
	}
	return domain.GuardianCoverageGap
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
