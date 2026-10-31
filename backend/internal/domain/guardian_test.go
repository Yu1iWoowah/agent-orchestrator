package domain

import (
	"math"
	"testing"
)

func TestA6GuardianCacheBucketsAndCounterValidation(t *testing.T) {
	ptr := func(v int64) *int64 { return &v }
	for _, tt := range []struct {
		name          string
		input, cached *int64
		valid         bool
		bucket        string
	}{
		{"missing", nil, nil, true, "unknown"},
		{"zero", ptr(0), ptr(0), true, "unknown"},
		{"negative", ptr(-1), ptr(0), false, "unknown"},
		{"invalid-cache", ptr(10), ptr(11), false, "unknown"},
		{"below", ptr(100), ptr(24), true, "below_25"},
		{"25", ptr(100), ptr(25), true, "25_to_75"},
		{"75", ptr(100), ptr(75), true, "75_to_95"},
		{"95", ptr(100), ptr(95), true, "at_least_95"},
		{"fraction-below", ptr(3), ptr(2), true, "25_to_75"},
		{"fraction-above", ptr(3), ptr(3), true, "at_least_95"},
		{"max", ptr(math.MaxInt64), ptr(math.MaxInt64), true, "at_least_95"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			v := GuardianTokens{Input: tt.input, Cached: tt.cached}
			if v.Valid() != tt.valid || v.CacheBucket() != tt.bucket {
				t.Fatalf("valid=%v bucket=%s", v.Valid(), v.CacheBucket())
			}
		})
	}
	// Independent recomputation: input contains cached and cache writes; output
	// contains reasoning. Neither subset is added a second time.
	vectors := []GuardianTokens{
		{Input: ptr(100), Cached: ptr(20), CacheWrite: ptr(10), Output: ptr(30), Reasoning: ptr(5)},
		{Input: ptr(200), Cached: ptr(150), CacheWrite: ptr(0), Output: ptr(40), Reasoning: ptr(10)},
	}
	var input, cached, uncached, output int64
	for _, v := range vectors {
		if !v.Valid() {
			t.Fatal("invalid fixture")
		}
		input += *v.Input
		cached += *v.Cached
		uncached += *v.Input - *v.Cached
		output += *v.Output
	}
	if input != 300 || cached != 170 || uncached != 130 || output != 70 || input != cached+uncached {
		t.Fatal("nonconserving counters")
	}
	if (GuardianTokens{Input: ptr(100), Cached: ptr(90), CacheWrite: ptr(11)}).Valid() {
		t.Fatal("cache write exceeds uncached")
	}
	if (GuardianTokens{Output: ptr(10), Reasoning: ptr(11)}).Valid() {
		t.Fatal("reasoning exceeds output")
	}
}
