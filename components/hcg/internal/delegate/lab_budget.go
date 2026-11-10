package delegate

import (
	"context"
	"path/filepath"
	"time"
)

// ApproveLabRun uses the original journal lock; no second balance or attempt
// counter exists. Re-registration can only preserve the grant or append binds.
func (b *Budget) ApproveLabRun(ctx context.Context, grant *LabGrant, period string, external map[string]ExternalSpend) error {
	if !grant.valid(b.OwnerRoot) || !grant.current(b.now()) || b.Dir != filepath.Join(b.OwnerRoot, "hcg", "delegation", "budget") {
		return ErrBudgetBlocked
	}
	return b.approvePeriod(ctx, period, external, grant)
}

func checkLabCapacity(s *budgetState, key string, now time.Time, root string) error {
	g := s.Grant
	if g == nil {
		return nil // The underlying monthly gate also has non-lab unit callers.
	}
	a, ok := s.Attempts[key]
	if !ok || !g.current(now) || g.OwnerRoot != root || a.LabRunID != g.RunID || !g.bound(a.Execution.ProjectID, a.Execution.SessionID) || g.Profiles[a.Profile.BillingProvider] != (ProfileRef{ID: a.Profile.ID, Revision: a.Profile.Revision}) {
		return ErrBudgetBlocked
	}
	totals := map[string]int64{}
	counts := map[string]int{}
	for _, attempt := range s.Attempts {
		if attempt.LabRunID != g.RunID {
			continue
		}
		family := attempt.Profile.BillingProvider
		if attempt.EverDispatched {
			counts[family]++
		}
		if attempt.State == "released" {
			continue
		}
		amount := attempt.Reserved
		if attempt.State == "settled" {
			if attempt.Charged == nil {
				return ErrBudgetBlocked
			}
			amount = *attempt.Charged
		}
		next, err := addMoney(totals[family], amount)
		if err != nil {
			return ErrBudgetBlocked
		}
		totals[family] = next
	}
	for _, family := range []string{"openai", "anthropic"} {
		if totals[family] > g.ProviderCapNanos[family] {
			return ErrBudgetBlocked
		}
	}
	total, err := addMoney(totals["openai"], totals["anthropic"])
	if err != nil || total > g.TotalCapNanos || counts[a.Profile.BillingProvider] >= g.AttemptsPerProvider {
		return ErrBudgetBlocked
	}
	return nil
}
