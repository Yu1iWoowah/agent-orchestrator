package delegate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

func TestPALCallerFaultPhaseCancellationBounds(t *testing.T) {
	if runtime.GOOS != "linux" || os.Getenv("HCG_PAL_TESTS") != "1" {
		t.Skip("isolated fixed PAL required")
	}
	for _, family := range []string{"openai", "anthropic"} {
		for _, point := range []string{"pal_assembly", "pal_ipc", "answer_validation"} {
			for _, deadline := range []bool{false, true} {
				t.Run(family+"/"+point+"/"+map[bool]string{false: "cancel", true: "deadline"}[deadline], func(t *testing.T) {
					root := t.TempDir()
					source := filepath.Join(t.TempDir(), "prompt.txt")
					body := "SYNTHETIC_SOURCE_142\nDOCUMENT_DIRECTIVE_IGNORE_THE_QUESTION\n"
					if err := os.WriteFile(source, []byte(body), 0600); err != nil {
						t.Fatal(err)
					}
					entered, release := make(chan struct{}), make(chan struct{})
					defer close(release)
					var calls atomic.Int32
					sink := &receiptSink{}
					c := Consumer{PAL: PAL{Python: os.Getenv("HCG_TEST_PAL_PYTHON"), Source: os.Getenv("HCG_TEST_PAL_SOURCE"), TempRoot: root}, Recorder: sink,
						Transport: transportFunc(func(context.Context, WireRequest) (WireResponse, error) {
							calls.Add(1)
							return providerFixtureReply("fixture-model", validProposal(body)), nil
						}),
						checkpoint: func(_ context.Context, at string) {
							if at == point {
								close(entered)
								<-release
							}
						},
					}
					profile := budgetProfile(family)
					request := approvedRequest(t, &c, "QUESTION_MUST_SURVIVE", source)
					ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
					defer cancel()
					type outcome struct {
						result Result
						err    error
					}
					done := make(chan outcome, 1)
					go func() {
						result, err := c.DelegateRead(ctx, testExecution, profile, request)
						done <- outcome{result, err}
					}()
					select {
					case <-entered:
					case <-time.After(5 * time.Second):
						t.Fatal("fixed PAL fault point not reached")
					}
					if !deadline {
						cancel()
					}
					select {
					case out := <-done:
						want := context.Canceled
						if deadline {
							want = context.DeadlineExceeded
						}
						if !errors.Is(out.err, want) || out.result.Reply.Answer != "" || out.result.text != "" {
							t.Fatalf("late answer or lost cancellation: %+v %v", out.result, out.err)
						}
					case <-time.After(3 * time.Second):
						t.Fatal("caller waited for uncooperative phase")
					}
					expected := int32(0)
					if point == "answer_validation" {
						expected = 1
					}
					sink.mu.Lock()
					receipts := len(sink.receipts)
					sink.mu.Unlock()
					if calls.Load() != expected || receipts != int(expected) {
						t.Fatalf("phase=%s calls=%d receipts=%d", point, calls.Load(), receipts)
					}
					entries, err := os.ReadDir(root)
					if err != nil || len(entries) != 0 {
						t.Fatalf("owned tool workspace not reaped: %v %v", entries, err)
					}
				})
			}
		}
	}
}
