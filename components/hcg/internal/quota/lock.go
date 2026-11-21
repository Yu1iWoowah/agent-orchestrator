package quota

import (
	"context"
	"fmt"
	"os"
	"time"
)

// The lock is held until this exact file descriptor is closed. All HCG log
// readers/writers participate; an unsupported lock must never permit a write.
func lockLog(ctx context.Context, f *os.File) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		locked, err := tryLogLock(f)
		if err != nil {
			return fmt.Errorf("quota log lock unavailable")
		}
		if locked {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}
