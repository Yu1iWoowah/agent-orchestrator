package domain

import (
	"errors"
	"regexp"
	"time"
)

// GuardianRetention is the metadata retention window, separate from billing.
const GuardianRetention = 30 * 24 * time.Hour

// GuardianCode is a closed vocabulary. Provider text must never become a code.
type GuardianCode string

// Fixed guardian anomaly codes never carry provider text.
const (
	GuardianInvalidMetadata    GuardianCode = "invalid_metadata"
	GuardianUnknownTime        GuardianCode = "skipped_unknown_time"
	GuardianOwnerUnresolved    GuardianCode = "owner_unresolved"
	GuardianOwnerConflict      GuardianCode = "owner_conflict"
	GuardianSourceConflict     GuardianCode = "source_conflict"
	GuardianSourceRotated      GuardianCode = "source_rotated"
	GuardianSourceTruncated    GuardianCode = "source_truncated"
	GuardianSourceMissing      GuardianCode = "source_missing"
	GuardianPartialLine        GuardianCode = "partial_line"
	GuardianCoverageGap        GuardianCode = "coverage_gap"
	GuardianUnsupportedVersion GuardianCode = "unsupported_version"
)

var (
	guardianID      = regexp.MustCompile(`^[A-Za-z0-9_-]{1,256}$`)
	guardianModel   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)
	guardianVersion = regexp.MustCompile(`^[0-9]{1,4}\.[0-9]{1,4}\.[0-9]{1,4}([.-][A-Za-z0-9.-]{1,32})?$`)
	// ErrGuardianCursorConflict rejects a stale atomic observation batch.
	ErrGuardianCursorConflict = errors.New("guardian cursor conflict")
	// ErrGuardianDisabled rejects collection after a policy or owner change.
	ErrGuardianDisabled = errors.New("guardian observation disabled")
)

// ValidGuardianID checks the bounded opaque provider ID vocabulary.
func ValidGuardianID(s string) bool { return guardianID.MatchString(s) }

// ValidGuardianModel checks a bounded model identifier.
func ValidGuardianModel(s string) bool { return guardianModel.MatchString(s) }

// ValidGuardianVersion checks a bounded CLI version identifier.
func ValidGuardianVersion(s string) bool { return guardianVersion.MatchString(s) }

// GuardianOwner is backed by an existing durable usage/account binding. Epoch
// is a database identity, retained across collector and daemon restarts.
type GuardianOwner struct {
	OwnershipConflict bool
	EpochID           int64
	BindingID         int64
	SessionID         SessionID
	ProjectID         ProjectID
	AccountID         string
	AccountBoundAt    time.Time
	NativeRootID      string
	EnabledAt         time.Time
	CleanedBefore     time.Time
}

// AcceptsProviderTime includes the enable/account boundary and excludes the
// purge/retention floor. Timestamp equality must not revive deleted facts.
func (o GuardianOwner) AcceptsProviderTime(at, now time.Time) bool {
	return !at.IsZero() && !at.Before(o.EnabledAt) && !at.Before(o.AccountBoundAt) && at.After(o.CleanedBefore) && at.After(now.Add(-GuardianRetention)) && !at.After(now)
}

// GuardianThread retains a proven guardian lineage and source version.
type GuardianThread struct {
	ProviderAt  time.Time
	EpochID     int64
	ThreadID    string
	ParentID    string
	CLIVersion  string
	FirstSeenAt time.Time
	LastSeenAt  time.Time
}

// GuardianTokens keeps missing counters distinct from reported zero.
type GuardianTokens struct {
	Input      *int64
	Cached     *int64
	CacheWrite *int64
	Output     *int64
	Reasoning  *int64
	Total      *int64
}

// Valid checks provider counters without arithmetic that could overflow.
func (v GuardianTokens) Valid() bool {
	for _, n := range []*int64{v.Input, v.Cached, v.CacheWrite, v.Output, v.Reasoning, v.Total} {
		if n != nil && *n < 0 {
			return false
		}
	}
	if v.Input != nil && v.Cached != nil && *v.Cached > *v.Input {
		return false
	}
	if v.Input != nil && v.CacheWrite != nil {
		limit := *v.Input
		if v.Cached != nil {
			limit -= *v.Cached
		}
		if *v.CacheWrite > limit {
			return false
		}
	}
	return v.Output == nil || v.Reasoning == nil || *v.Reasoning <= *v.Output
}

// CacheBucket uses integer thresholds without multiplying int64 counters.
func (v GuardianTokens) CacheBucket() string {
	if !v.Valid() || v.Input == nil || v.Cached == nil || *v.Input == 0 {
		return "unknown"
	}
	// ceil(input * percentage / 100), split before multiplying to avoid overflow.
	threshold := func(p int64) int64 { return *v.Input/100*p + (*v.Input%100*p+99)/100 }
	switch {
	case *v.Cached < threshold(25):
		return "below_25"
	case *v.Cached < threshold(75):
		return "25_to_75"
	case *v.Cached < threshold(95):
		return "75_to_95"
	default:
		return "at_least_95"
	}
}

// GuardianRequest is one deduplicated response, independent of the usage ledger.
type GuardianRequest struct {
	CLIVersion       string
	EpochID          int64
	ThreadID         string
	TurnID           string
	ResponseID       string
	RootTurnID       string
	ModelID          string // empty means missing; source is always exact turn_context
	Tokens           GuardianTokens
	ProviderAt       time.Time
	FirstSeenAt      time.Time
	SourceGeneration int64
	SourceOffset     int64
	Conflict         bool
}

// GuardianTurnModel is the entire pending model queue: no raw context survives.
type GuardianTurnModel struct {
	TurnID     string
	ModelID    string
	ProviderAt time.Time
}

// GuardianCursor is a metadata-only read position without provider content.
type GuardianCursor struct {
	SourceSize       int64
	SourceModifiedAt time.Time
	EpochID          int64
	ThreadID         string
	Generation       int64
	Offset           int64
	FileIdentity     string
	Discarding       bool
	UpdatedAt        time.Time
}

// GuardianBatch commits facts and both source positions as one transaction.
type GuardianBatch struct {
	Owner            GuardianOwner
	Thread           GuardianThread
	Expected         GuardianCursor
	ExpectedPosition GuardianCursor
	Cursor           GuardianCursor
	Requests         []GuardianRequest
	Models           []GuardianTurnModel
	Anomalies        map[GuardianCode]int64
}
