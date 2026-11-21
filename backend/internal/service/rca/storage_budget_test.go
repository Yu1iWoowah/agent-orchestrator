package rca

import (
	"math"
	"testing"
)

func TestSpaceThresholdAndPlannedPeak(t *testing.T) {
	const gib = uint64(1 << 30)
	for _, tc := range []struct {
		name              string
		total, free, peak uint64
		ok                bool
	}{
		{"below", 100 * gib, 2*gib - 1, 0, false},
		{"equal", 100 * gib, 2 * gib, 0, true},
		{"planned below", 100 * gib, 2 * gib, 1, false},
		{"planned equal", 100 * gib, 2*gib + 4096, 4096, true},
		{"one percent", 400 * gib, 4 * gib, 0, true},
		{"rounded percent", 400*gib + 1, 4 * gib, 0, false},
		{"invalid total", 0, 0, 0, false},
		{"invalid free", gib, 2 * gib, 0, false},
		{"peak overflow", 100 * gib, 3 * gib, math.MaxUint64, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := spaceAllows(volumeSpace{Total: tc.total, Available: tc.free, Identity: "fixture"}, tc.peak); got != tc.ok {
				t.Fatalf("space admission=%v", got)
			}
		})
	}
	if spaceAllows(volumeSpace{Total: 100 * gib, Available: 3 * gib}, 0) {
		t.Fatal("unknown volume admitted")
	}
}
