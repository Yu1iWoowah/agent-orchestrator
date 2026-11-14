package delegate

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"time"
)

type LabSession struct {
	ProjectID string `json:"projectId"`
	SessionID string `json:"sessionId"`
}

// LabGrant is operator-only registration data, never a model tool argument.
// One immutable run owns this root; only its explicit session list may grow.
type LabGrant struct {
	RunID               string                `json:"runId"`
	OwnerRoot           string                `json:"ownerRoot"`
	CanonicalOwner      LabSession            `json:"canonicalOwner"`
	StartsAt            time.Time             `json:"startsAt"`
	ExpiresAt           time.Time             `json:"expiresAt"`
	Profiles            map[string]ProfileRef `json:"profiles"`
	Bindings            []LabSession          `json:"bindings"`
	ProviderCapNanos    map[string]int64      `json:"providerCapNanos"`
	TotalCapNanos       int64                 `json:"totalCapNanos"`
	AttemptsPerProvider int                   `json:"attemptsPerProvider"`
}

func (g *LabGrant) valid(root string) bool {
	if g == nil || !profileToken.MatchString(g.RunID) || !filepath.IsAbs(root) || filepath.Clean(root) != root || g.OwnerRoot != root || g.CanonicalOwner.ProjectID == "" || g.CanonicalOwner.SessionID == "" || len(g.Profiles) != 2 || len(g.ProviderCapNanos) != 2 || len(g.Bindings) == 0 || len(g.Bindings) > 256 || g.TotalCapNanos != 20*nanoUSD || g.AttemptsPerProvider != 26 || !g.ExpiresAt.After(g.StartsAt) || g.ExpiresAt.Sub(g.StartsAt) > 24*time.Hour || g.StartsAt.UTC().Format("2006-01") != g.ExpiresAt.UTC().Format("2006-01") {
		return false
	}
	for _, family := range []string{"openai", "anthropic"} {
		p := g.Profiles[family]
		if !profileToken.MatchString(p.ID) || !profileToken.MatchString(p.Revision) || g.ProviderCapNanos[family] != 10*nanoUSD {
			return false
		}
	}
	seen := map[string]bool{}
	for _, b := range g.Bindings {
		if b.ProjectID == "" || b.SessionID == "" || !safeText(b.ProjectID) || !safeText(b.SessionID) || seen[b.SessionID] {
			return false
		}
		seen[b.SessionID] = true
	}
	return g.bound(g.CanonicalOwner.ProjectID, g.CanonicalOwner.SessionID)
}

func (g *LabGrant) current(now time.Time) bool {
	return g != nil && !now.Before(g.StartsAt) && now.Before(g.ExpiresAt)
}

func (g *LabGrant) bound(project, session string) bool {
	if g != nil {
		for _, b := range g.Bindings {
			if b.ProjectID == project && b.SessionID == session {
				return true
			}
		}
	}
	return false
}

func mergeLabGrant(old, next *LabGrant) (*LabGrant, error) {
	if old != nil {
		a, b := *old, *next
		a.Bindings, b.Bindings = nil, nil
		if !reflect.DeepEqual(a, b) {
			return nil, ErrUnavailable
		}
		for _, binding := range old.Bindings {
			if !next.bound(binding.ProjectID, binding.SessionID) {
				return nil, ErrUnavailable
			}
		}
	}
	var frozen LabGrant
	raw, _ := json.Marshal(next)
	if json.Unmarshal(raw, &frozen) != nil {
		return nil, ErrUnavailable
	}
	return &frozen, nil
}

func (c *Coordinator) LabGrant() *LabGrant {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state.Grant == nil {
		return nil
	}
	g, _ := mergeLabGrant(nil, c.state.Grant)
	return g
}

// CheckLabStartup runs under the daemon data lock, before sqlite.Open could
// create an empty replacement for a canonical owner whose metadata survives.
func CheckLabStartup(ctx context.Context, root string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, name := range []string{"registry.json", "budget/budget.json"} {
		_, err := os.Lstat(filepath.Join(root, "hcg", "delegation", name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return ErrUnavailable
		}
		db, err := os.Lstat(filepath.Join(root, "ao.db"))
		if err != nil || !db.Mode().IsRegular() {
			return ErrUnavailable
		}
	}
	return nil
}
