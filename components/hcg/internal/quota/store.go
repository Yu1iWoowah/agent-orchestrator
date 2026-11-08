package quota

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Store is an append-only sampling log, not a token ledger. Root must be the
// explicitly selected AO namespace; there is no HOME or stable-data fallback.
type Store struct{ Root string }

func (s Store) path() (string, error) {
	if s.Root == "" || !filepath.IsAbs(s.Root) {
		return "", fmt.Errorf("explicit absolute AO data root required")
	}
	return filepath.Join(s.Root, "hcg", "quota", "samples.jsonl"), nil
}
func (s Store) Append(ctx context.Context, sample Sample) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := s.path()
	if err != nil {
		return err
	}
	if sample.DataRoot != s.Root {
		return fmt.Errorf("quota namespace mismatch")
	}
	// Refuse symlinks in the HCG-owned path rather than redirecting a write.
	for _, dir := range []string{filepath.Join(s.Root, "hcg"), filepath.Dir(path)} {
		if err := os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
			return fmt.Errorf("quota directory unavailable")
		}
		info, err := os.Lstat(dir)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("quota directory unavailable")
		}
	}
	if info, err := os.Lstat(path); err == nil && (!info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0) {
		return fmt.Errorf("quota log is not a regular file")
	}
	raw, err := json.Marshal(sample)
	if err != nil {
		return fmt.Errorf("invalid quota sample")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return fmt.Errorf("quota log unavailable")
	}
	_, writeErr := f.Write(append(raw, '\n'))
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		return fmt.Errorf("quota sample write failed")
	}
	return nil
}
func (s Store) Read(ctx context.Context) ([]Sample, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := s.path()
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return []Sample{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("quota log unavailable")
	}
	defer f.Close()
	rows := []Sample{}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 4<<20)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var sample Sample
		if json.Unmarshal(scanner.Bytes(), &sample) != nil || sample.DataRoot != s.Root || sample.SampledAt.IsZero() {
			return nil, fmt.Errorf("invalid quota log")
		}
		rows = append(rows, sample)
	}
	if scanner.Err() != nil {
		return nil, fmt.Errorf("incomplete quota log")
	}
	return rows, nil
}
