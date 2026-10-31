package guardian

import (
	"bufio"
	"context"
	"errors"
	"io"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// verifyPrefix uses the budget left after forward ingestion. Growth does not
// prove append-only history, so this durable pass also visits unrelated lines.
func (s *Service) verifyPrefix(ctx context.Context, f io.ReadSeeker, b *domain.GuardianBatch, records int, bytes int64) error {
	c := &b.Cursor
	if !c.ReplayDiscarding {
		partial, err := startsInsideLine(ctx, f, c.ReplayOffset)
		if err != nil {
			return err
		}
		c.ReplayDiscarding = partial
		if partial && c.ReplayOffset > c.ReadStartOffset {
			c.ReplayAgain = true
		}
	}
	if _, err := f.Seek(c.ReplayOffset, io.SeekStart); err != nil {
		return err
	}
	// A rewrite can move the saved endpoint into a line. Stop after that line,
	// under the existing budgets, rather than manufacturing EOF at the old byte.
	reader := bufio.NewReaderSize(f, maxLine+1)
	probe := domain.GuardianBatch{Owner: b.Owner, Thread: b.Thread, Cursor: *c, Anomalies: map[domain.GuardianCode]int64{}}
	for n := 0; n < records && bytes > 0 && c.ReplayOffset < c.ReplayUntil; n++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		offset := c.ReplayOffset
		line, err := reader.ReadSlice('\n')
		bytes -= int64(len(line))
		if errors.Is(err, io.EOF) {
			end := c.ReplayOffset + int64(len(line))
			if c.ReplayDiscarding {
				c.ReplayOffset += int64(len(line))
			}
			if end < c.ReplayUntil {
				probe.Anomalies[domain.GuardianSourceTruncated]++
				probe.Anomalies[domain.GuardianCoverageGap]++
			} else if len(line) > 0 && !c.ReplayDiscarding {
				probe.Anomalies[domain.GuardianPartialLine]++
			}
			break
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			c.ReplayOffset += int64(len(line))
			c.ReplayDiscarding = true
			probe.Anomalies[domain.GuardianInvalidMetadata]++
			continue
		}
		if err != nil {
			return err
		}
		c.ReplayOffset += int64(len(line))
		if c.ReplayDiscarding {
			c.ReplayDiscarding = false
		} else if len(line) > maxLine {
			probe.Anomalies[domain.GuardianInvalidMetadata]++
		} else {
			appendRecord(&probe, line, offset)
		}
	}
	if len(probe.Requests) > 0 {
		if err := s.revalidationGap(ctx, &probe, c.ReplayUntil); err != nil {
			return err
		}
	}
	b.Requests = append(b.Requests, probe.Requests...)
	b.Models = append(b.Models, probe.Models...)
	for code, count := range probe.Anomalies {
		b.Anomalies[code] += count
	}
	if c.ReplayOffset >= c.ReplayUntil {
		finishRevalidation(c)
	}
	return nil
}

func startsInsideLine(ctx context.Context, f io.ReadSeeker, offset int64) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if offset == 0 {
		return false, nil
	}
	if _, err := f.Seek(offset-1, io.SeekStart); err != nil {
		return false, err
	}
	var previous [1]byte
	_, err := io.ReadFull(f, previous[:])
	return previous[0] != '\n', err
}

func finishRevalidation(c *domain.GuardianCursor) {
	if c.ReplayAgain {
		c.ReplayOffset = c.ReadStartOffset
		c.ReplayUntil = c.Offset
	} else {
		c.ReplayOffset = 0
		c.ReplayUntil = 0
	}
	c.ReplayAgain = false
	c.ReplayDiscarding = false
}
