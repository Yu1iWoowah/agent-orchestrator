package claudecode

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"os"
)

const transcriptTailWindow = 64 << 10

// transcriptTail keeps bounded bytes from the stream actually parsed.
type transcriptTail struct{ data []byte }

func (t *transcriptTail) Write(p []byte) (int, error) {
	n := len(p)
	if n >= transcriptTailWindow {
		t.data = append(t.data[:0], p[n-transcriptTailWindow:]...)
		return n, nil
	}
	if excess := len(t.data) + n - transcriptTailWindow; excess > 0 {
		copy(t.data, t.data[excess:])
		t.data = t.data[:len(t.data)-excess]
	}
	t.data = append(t.data, p...)
	return n, nil
}

func nativeStabilityCheck(rootPath, relative string, rootInfo, expected os.FileInfo, tail [32]byte) func(context.Context) error {
	return func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		changed := func() error {
			return transcriptFailure("TRANSCRIPT_SOURCE_CHANGED", "native file fingerprint changed after preflight")
		}
		root, err := os.OpenRoot(rootPath)
		if err != nil {
			return changed()
		}
		defer func() { _ = root.Close() }()
		currentRoot, err := root.Stat(".")
		if err != nil || !os.SameFile(rootInfo, currentRoot) {
			return changed()
		}
		matches := func(info os.FileInfo) bool {
			return info != nil && info.Mode().IsRegular() && os.SameFile(expected, info) && info.Size() == expected.Size() && info.ModTime().Equal(expected.ModTime())
		}
		named, err := root.Lstat(relative)
		if err != nil || !matches(named) {
			return changed()
		}
		file, err := root.Open(relative)
		if err != nil {
			return changed()
		}
		defer func() { _ = file.Close() }()
		opened, err := file.Stat()
		if err != nil || !matches(opened) {
			return changed()
		}
		length := min(expected.Size(), int64(transcriptTailWindow))
		h := sha256.New()
		n, err := io.Copy(h, io.NewSectionReader(file, expected.Size()-length, length))
		if err != nil || n != length || !bytes.Equal(h.Sum(nil), tail[:]) {
			return changed()
		}
		after, err := file.Stat()
		if err != nil || !matches(after) {
			return changed()
		}
		named, err = root.Lstat(relative)
		if err != nil || !matches(named) {
			return changed()
		}
		namedRoot, err := os.Stat(rootPath)
		if err != nil || !os.SameFile(rootInfo, namedRoot) {
			return changed()
		}
		return ctx.Err()
	}
}
