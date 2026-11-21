package store

import (
	"context"
	"fmt"
	"path/filepath"
)

// CanonicalDataRoot identifies the main DB actually used by this store. It
// creates no identity, settings or replacement DB and includes no credentials.
func (s *Store) CanonicalDataRoot(ctx context.Context) (string, error) {
	var path string
	if err := s.readDB.QueryRowContext(ctx, "SELECT file FROM pragma_database_list WHERE name = 'main'").Scan(&path); err != nil {
		return "", fmt.Errorf("read canonical data root: %w", err)
	}
	if !filepath.IsAbs(path) || filepath.Base(path) != "ao.db" {
		return "", fmt.Errorf("canonical database is unavailable")
	}
	return filepath.Dir(path), nil
}
