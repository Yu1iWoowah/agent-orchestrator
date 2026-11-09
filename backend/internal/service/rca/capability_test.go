package rca

import "testing"

func TestApprovedBaselineIsSeparateAndDigestChecked(t *testing.T) {
	got := baseline(approvedBaseline)
	if got.Status != "baseline_non_request" || measured(got.Bytes) != 83412 || measured(got.EstimatedTokens) != 20853 {
		t.Fatal("approved numeric baseline missing")
	}
	for _, bad := range [][]byte{nil, []byte("{}")} {
		got := baseline(bad)
		if got.Status != "unknown" || got.Bytes != nil || got.EstimatedTokens != nil {
			t.Fatal("missing evidence became measured zero")
		}
	}
}
