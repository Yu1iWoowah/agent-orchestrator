package quota

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Store is an append-only sampling log, not a token ledger. Root must be the
// explicitly selected AO namespace; there is no HOME or stable-data fallback.
type Store struct {
	Root            string
	afterValidation func() // Test seam at the validation/write boundary.
}

func (s Store) path() (string, error) {
	if s.Root == "" || !filepath.IsAbs(s.Root) {
		return "", fmt.Errorf("explicit absolute AO data root required")
	}
	return filepath.Join(s.Root, "hcg", "quota", "samples.jsonl"), nil
}
func (s Store) Append(ctx context.Context, sample Sample) (err error) {
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
	// One descriptor and one exclusive lock cover validation, the append and
	// Close. A waiter revalidates after a failed writer releases its lock.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0600)
	if err != nil {
		return fmt.Errorf("quota log unavailable")
	}
	defer func() {
		if closeErr := f.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("quota log close failed")
		}
	}()
	if err := lockLog(ctx, f); err != nil {
		return err
	}
	if _, err := scanLog(ctx, f, s.Root, false); err != nil {
		return err
	}
	if s.afterValidation != nil {
		s.afterValidation()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := f.Write(append(raw, '\n')); err != nil {
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
	if err := lockLog(ctx, f); err != nil {
		return nil, err
	}
	return scanLog(ctx, f, s.Root, true)
}

func scanLog(ctx context.Context, reader io.Reader, root string, collect bool) ([]Sample, error) {
	rows := []Sample{}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 4<<20)
	scanner.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			return i + 1, data[:i], nil
		}
		if atEOF && len(data) > 0 {
			return 0, nil, fmt.Errorf("incomplete quota log")
		}
		return 0, nil, nil
	})
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var sample Sample
		if json.Unmarshal(scanner.Bytes(), &sample) != nil || sample.DataRoot != root || sample.SampledAt.IsZero() {
			return nil, fmt.Errorf("invalid quota log")
		}
		if collect {
			rows = append(rows, sample)
		}
	}
	if scanner.Err() != nil {
		return nil, fmt.Errorf("incomplete quota log")
	}
	return rows, nil
}
