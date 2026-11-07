package chat

import (
	"encoding/json"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestSideProtectionProjection(t *testing.T) {
	svc := New(Options{})
	t.Cleanup(svc.sides.cancel)
	for _, tc := range []struct {
		name             string
		state            domain.SideCompactionState
		closed, isolated bool
	}{
		{"pending", domain.SideCompactionPending, false, true},
		{"unknown", domain.SideCompactionUnknown, false, true},
		{"closed", domain.SideCompactionUnknown, true, false},
		{"settled", domain.SideCompactionSettled, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := domain.SideCompaction{State: tc.state, Closed: tc.closed, Provider: domain.HarnessCodex, Content: json.RawMessage(`{}`)}
			before := row
			p := svc.sideProtectionStatus(t.Context(), row)
			if p.Result != tc.state || p.Isolated != tc.isolated || p.ShutdownBlocked != tc.isolated || p.Closed != tc.closed {
				t.Fatalf("status: %+v", p)
			}
			if tc.isolated && p.Reason == "" {
				t.Fatal("missing shutdown reason")
			}
			if row.State != before.State || row.Historical != before.Historical {
				t.Fatal("projection changed result")
			}
		})
	}
}

func TestProtectedSideStatusMatchesPreflight(t *testing.T) {
	svc, db, _, _ := newRecordHarness(t, domain.HarnessCodex)
	_, _ = svc.CompactSideChat(t.Context(), "session", "side")
	sides, err := svc.ListProtectedSides(t.Context())
	if err != nil || len(sides) != 1 || !sides[0].Protection.Isolated || !sides[0].Protection.ShutdownBlocked {
		t.Fatalf("sides=%+v err=%v", sides, err)
	}
	rows, err := db.SideCompactions(t.Context(), true)
	if err != nil || len(rows) != 1 || rows[0].Historical {
		t.Fatal("read mutated B record", rows, err)
	}
	if err := svc.CloseProtectedSide(t.Context(), "side"); err != nil {
		t.Fatal(err)
	}
	sides, err = svc.ListProtectedSides(t.Context())
	if err != nil || len(sides) != 0 {
		t.Fatal("closed side still listed", sides, err)
	}
}
