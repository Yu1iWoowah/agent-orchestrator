package delegate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
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

// checkJournal never creates or repairs metadata. A missing owner journal is
// not a fresh balance, and incomplete registration cannot become a new run.
func (c *Coordinator) checkJournal(ctx context.Context) (budgetState, error) {
	c.mu.Lock()
	grant, history, invalid := c.state.Grant, c.state.History, c.loadErr != nil || c.closed
	c.mu.Unlock()
	if invalid {
		return budgetState{}, ErrUnavailable
	}
	state, err := c.budget.readState(ctx)
	if errors.Is(err, os.ErrNotExist) && grant == nil {
		return budgetState{}, nil
	}
	if err != nil || !reflect.DeepEqual(grant, state.Grant) {
		return budgetState{}, ErrBudgetBlocked
	}
	if grant != nil {
		info, err := os.Lstat(filepath.Join(c.dir, "registry.json"))
		if err != nil || !info.Mode().IsRegular() || grant.OwnerRoot != c.root {
			return budgetState{}, ErrBudgetBlocked
		}
	}
	for _, a := range state.Attempts {
		if a.LabRunID != "" {
			d, ok := history[a.ProfileDigest]
			if !ok || d.Profile != a.Profile {
				return budgetState{}, ErrBudgetBlocked
			}
		}
	}
	return state, nil
}

func (c *Coordinator) prepareRegistration(ctx context.Context) error {
	if _, err := c.checkJournal(ctx); err != nil {
		return err
	}
	c.mu.Lock()
	resume := !c.grantReady
	c.mu.Unlock()
	if !resume {
		return nil
	}
	if err := c.RecoverReceipts(ctx); err != nil {
		return err
	}
	state, err := c.checkJournal(ctx)
	if err != nil {
		return err
	}
	for _, a := range state.Attempts {
		if a.State != "settled" && a.State != "released" {
			return ErrBudgetBlocked
		}
	}
	return nil
}
