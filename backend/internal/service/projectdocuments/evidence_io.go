package projectdocuments

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
)

func openEvidenceFile(r *os.Root, p string) (*os.File, error) {
	info, e := r.Lstat(p)
	if e != nil || !info.Mode().IsRegular() {
		return nil, conflict("HANDOFF_EVIDENCE_CORRUPT", "Evidence is not a regular file")
	}
	f, e := r.Open(p)
	if e != nil {
		return nil, e
	}
	after, e := f.Stat()
	if e != nil || !os.SameFile(info, after) {
		_ = f.Close()
		return nil, conflict("HANDOFF_EVIDENCE_CORRUPT", "Evidence file identity changed")
	}
	return f, nil
}
func verifyEvidence(ctx context.Context, f *os.File, want string, size, limit int64) error {
	info, e := f.Stat()
	if e != nil {
		return e
	}
	if info.Size() > limit || size >= 0 && info.Size() != size {
		return conflict("HANDOFF_EVIDENCE_CORRUPT", "Evidence size differs from its receipt")
	}
	h := sha256.New()
	buf := make([]byte, 64<<10)
	var read int64
	for {
		if e := ctx.Err(); e != nil {
			return e
		}
		n, err := f.Read(buf)
		if n > 0 {
			read += int64(n)
			if read > limit {
				return conflict("HANDOFF_EVIDENCE_CORRUPT", "Evidence exceeds its bound")
			}
			_, _ = h.Write(buf[:n])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	if read != info.Size() || hex.EncodeToString(h.Sum(nil)) != want {
		return conflict("HANDOFF_EVIDENCE_CORRUPT", "Evidence hash differs from its receipt")
	}
	_, e = f.Seek(0, io.SeekStart)
	return e
}
func compareEvidenceFile(ctx context.Context, r *os.Root, p, h string, size int64) error {
	f, e := openEvidenceFile(r, p)
	if e != nil {
		return e
	}
	defer func() { _ = f.Close() }()
	return verifyEvidence(ctx, f, h, size, 512<<20)
}
func duplicateEvidenceDescriptor(ctx context.Context, r *os.Root, p, h string, size, limit int64) (*os.File, error) {
	f, e := openEvidenceFile(r, p)
	if e != nil {
		return nil, e
	}
	if e := verifyEvidence(ctx, f, h, size, limit); e != nil {
		_ = f.Close()
		return nil, e
	}
	return f, nil
}
