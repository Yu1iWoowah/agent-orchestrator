package claudecode

import (
	"context"
	"errors"
	"os"
	"runtime"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type missingNativeStat struct{ os.FileInfo }

func (missingNativeStat) Sys() any { return nil }

func TestNativeStabilityMissingChangeTimeFailsClosed(t *testing.T) {
	info, err := os.Stat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	check, available := nativeStabilityCheck("unused", "unused", nil, missingNativeStat{info}, [32]byte{})
	if available {
		t.Fatal("missing native stat accepted as a verified witness")
	}
	var failure *ports.NativeTranscriptError
	if err = check(context.Background()); !errors.As(err, &failure) || failure.Code != "TRANSCRIPT_STABILITY_UNSUPPORTED" {
		t.Fatalf("missing change time did not fail closed: %v", err)
	}
}

func TestNativeTranscriptPlatformStabilityPolicy(t *testing.T) {
	req := transcriptFixture(t, []map[string]any{mainRow("u", "", "user", "question"), mainRow("a", "u", "assistant", "answer")})
	segment, err := New().ReadNativeTranscript(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if segment.CheckStability == nil {
		t.Fatal("native reader omitted explicit publication policy")
	}
	err = segment.CheckStability(context.Background())
	if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
		if err != nil {
			t.Fatal(err)
		}
	} else {
		var failure *ports.NativeTranscriptError
		if !errors.As(err, &failure) || failure.Code != "TRANSCRIPT_STABILITY_UNSUPPORTED" {
			t.Fatalf("unsupported platform silently accepted publication: %v", err)
		}
	}
}
