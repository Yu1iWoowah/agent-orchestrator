package integration

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

type countedPreprocessingReader struct{ remaining, reads int }

func (r *countedPreprocessingReader) Read(p []byte) (int, error) {
	r.reads++
	if r.remaining == 0 {
		return 0, io.EOF
	}
	n := len(p)
	if n > r.remaining {
		n = r.remaining
	}
	clear(p[:n])
	r.remaining -= n
	return n, nil
}

type cancellingPreprocessingWriter struct {
	cancel context.CancelFunc
	writes int
}

func (w *cancellingPreprocessingWriter) Write(p []byte) (int, error) {
	w.writes++
	w.cancel()
	return len(p), nil
}
func TestPreprocessingCopyStopsReadingAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	reader := &countedPreprocessingReader{remaining: 256 << 10}
	writer := &cancellingPreprocessingWriter{cancel: cancel}
	_, err := copyWithContext(ctx, writer, reader)
	if !errors.Is(err, context.Canceled) || reader.reads != 1 || writer.writes != 1 {
		t.Fatalf("cancel did not stop preprocessing reads: reads=%d writes=%d err=%v", reader.reads, writer.writes, err)
	}
}
func TestReportPreprocessingPreservesCancellationReason(t *testing.T) {
	id := testIdentity(t.TempDir())
	engine := Engine{Executable: "/missing/node_modules/llm-usage-metrics/dist/bin.js", NodeExecutable: "/missing/node", Version: EngineVersion, Digest: strings.Repeat("a", 64)}
	m, err := LaunchManifest(nil, id, PolicyInputs{Project: boolean(true)}, &engine)
	if err != nil {
		t.Fatal(err)
	}
	for _, deadline := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancelled", true: "deadline_exceeded"}[deadline], func(t *testing.T) {
			var ctx context.Context
			var cancel context.CancelFunc
			if deadline {
				ctx, cancel = context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
			} else {
				ctx, cancel = context.WithCancel(t.Context())
				cancel()
			}
			defer cancel()
			out := (Runner{TempRoot: t.TempDir()}).Report(ctx, m, []AuthorizedSource{{Root: t.TempDir(), Path: "native.jsonl", Source: "claude", NativeID: id.NativeID}})
			want := "cancelled"
			if deadline {
				want = "deadline_exceeded"
			}
			if out.Status != "unavailable" || out.Reason != want {
				t.Fatalf("preprocessing cancellation relabelled as deployment change: %+v", out)
			}
		})
	}
}
