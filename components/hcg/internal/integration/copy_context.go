package integration

import (
	"context"
	"io"
)

// copyWithContext keeps file hashing and source staging cancellable between
// bounded reads. Do not let io.Copy's WriterTo/ReaderFrom fast paths bypass it.
func copyWithContext(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	buf := make([]byte, 32<<10)
	var written int64
	for {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		n, readErr := src.Read(buf)
		if err := ctx.Err(); err != nil {
			return written, err
		}
		if n > 0 {
			nw, err := dst.Write(buf[:n])
			written += int64(nw)
			if err != nil {
				return written, err
			}
			if nw != n {
				return written, io.ErrShortWrite
			}
		}
		if err := ctx.Err(); err != nil {
			return written, err
		}
		if readErr != nil {
			if readErr == io.EOF {
				return written, nil
			}
			return written, readErr
		}
	}
}
