package ports

import (
	"context"
	"io"
)

// RCAInlineAdmission is an already-reserved, native-only raw reader. Release
// belongs to the physical transport after reads stop, not request cancellation.
type RCAInlineAdmission interface {
	Context() context.Context
	Read(io.Reader, int64) string
	Release()
}
