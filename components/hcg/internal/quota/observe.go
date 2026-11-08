// Package quota records projected account CLI numbers, never provider payloads.
package quota

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"
)

type Window struct {
	ID          string     `json:"id"`
	Label       string     `json:"label"`
	UsedPercent *float64   `json:"usedPercent"`
	ResetsAt    *time.Time `json:"resetsAt"`
}
type Account struct {
	ID        string     `json:"id"`
	Provider  string     `json:"provider"`
	FetchedAt *time.Time `json:"fetchedAt"`
	Status    string     `json:"status"`
	Reason    string     `json:"reason,omitempty"`
	Windows   []Window   `json:"windows"`
}
type Sample struct {
	DataRoot  string    `json:"dataRoot"`
	SampledAt time.Time `json:"sampledAt"`
	Status    string    `json:"status"`
	Reason    string    `json:"reason,omitempty"`
	Accounts  []Account `json:"accounts"`
}

// Project is the only raw-CLI boundary. Unknown fields and raw errors are dropped.
func Project(raw []byte, cliErr error, root string, at time.Time) Sample {
	out := Sample{DataRoot: root, SampledAt: at.UTC(), Status: "error", Accounts: []Account{}}
	if cliErr != nil {
		out.Reason = "account_cli_failed"
		return out
	}
	var wire struct {
		Accounts *[]struct {
			ID       string `json:"id"`
			Provider string `json:"provider"`
			Problem  string `json:"problem"`
			Limits   *struct {
				Windows   []Window   `json:"windows"`
				FetchedAt *time.Time `json:"fetchedAt"`
			} `json:"limits"`
		} `json:"accounts"`
	}
	if json.Unmarshal(raw, &wire) != nil || wire.Accounts == nil {
		out.Reason = "invalid_account_json"
		return out
	}
	seen := map[string]bool{}
	for _, a := range *wire.Accounts {
		key := a.Provider + "/" + a.ID
		if a.ID == "" || a.Provider == "" || seen[key] {
			out.Accounts = []Account{}
			out.Reason = "invalid_account_identity"
			return out
		}
		seen[key] = true
		row := Account{ID: a.ID, Provider: a.Provider, Status: "unavailable", Reason: "limits_missing", Windows: []Window{}}
		if a.Limits != nil {
			row.FetchedAt = a.Limits.FetchedAt
			row.Windows = a.Limits.Windows
			if row.Windows == nil {
				row.Windows = []Window{}
			}
			row.Status, row.Reason = "available", ""
			if row.FetchedAt == nil || row.FetchedAt.IsZero() {
				row.FetchedAt = nil
				row.Status, row.Reason = "unavailable", "fetched_at_missing"
			}
			windows := map[string]bool{}
			for i, w := range row.Windows {
				if w.ID == "" || windows[w.ID] {
					out.Accounts = []Account{}
					out.Reason = "invalid_window_identity"
					return out
				}
				windows[w.ID] = true
				if w.UsedPercent != nil && (math.IsNaN(*w.UsedPercent) || math.IsInf(*w.UsedPercent, 0) || *w.UsedPercent < 0 || *w.UsedPercent > 100) {
					row.Windows[i].UsedPercent = nil
				}
				if w.ResetsAt != nil && w.ResetsAt.IsZero() {
					row.Windows[i].ResetsAt = nil
				}
			}
			if len(row.Windows) == 0 {
				row.Status, row.Reason = "unavailable", "windows_missing"
			}
		}
		if a.Problem != "" {
			row.Status, row.Reason = "unavailable", "account_unavailable"
		}
		out.Accounts = append(out.Accounts, row)
	}
	out.Status = "success"
	return out
}

// Observe waits after each completed tick, so a slow CLI never overlaps itself
// or causes a backlog of catch-up samples. A cancelled context stops the loop.
func Observe(ctx context.Context, interval time.Duration, tick func(context.Context) error) error {
	if interval <= 0 {
		return fmt.Errorf("interval must be positive")
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := tick(ctx); err != nil {
			return err
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
