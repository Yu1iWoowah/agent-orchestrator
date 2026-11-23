package delegate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
)

func (b *Budget) readState(ctx context.Context) (budgetState, error) {
	info, err := os.Lstat(filepath.Join(b.Dir, "budget.json"))
	if err != nil {
		return budgetState{}, err
	}
	if !info.Mode().IsRegular() {
		return budgetState{}, ErrBudgetBlocked
	}
	var state budgetState
	err = b.withState(ctx, func(s *budgetState) error { state = *s; return nil }, nil, false)
	return state, err
}

// checkJournal never creates or repairs metadata.
func (c *Coordinator) checkJournal(ctx context.Context) (budgetState, error) {
	c.mu.Lock()
	invalid := c.loadErr != nil || c.closed
	c.mu.Unlock()
	if invalid {
		return budgetState{}, ErrUnavailable
	}
	state, err := c.budget.readState(ctx)
	if errors.Is(err, os.ErrNotExist) {
		return budgetState{}, nil
	}
	if err != nil {
		return budgetState{}, ErrBudgetBlocked
	}
	return state, nil
}
