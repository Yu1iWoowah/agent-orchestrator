package orchestratorreplacement

import (
	"context"
	"errors"
	"fmt"
)

// Reconcile advances each durable operation by one boundary. Waiting operations
// remain visible until an explicit retry; one storage failure does not hide the
// other projects' work. Provider actions retain Advance's project reservation.
func (s *Service) Reconcile(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	ops, err := s.deps.Store.ListActiveOrchestratorReplacements(ctx)
	if err != nil {
		return fmt.Errorf("list active replacements: %w", err)
	}
	var failures []error
	for _, op := range ops {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		if _, err := s.Advance(ctx, op.ID); err != nil {
			failures = append(failures, fmt.Errorf("reconcile replacement %s: %w", op.ID, err))
		}
	}
	return errors.Join(failures...)
}
