package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestSideCompactionTransitionsAndContent(t *testing.T) {
	ctx := context.Background()
	for _, state := range []domain.SideCompactionState{domain.SideCompactionSettled, domain.SideCompactionNotSent, domain.SideCompactionUnknown} {
		t.Run(string(state), func(t *testing.T) {
			s := newTestStore(t)
			r := domain.SideCompaction{ID: "command", SideID: "side", NativeHome: "/home/provider", NativeID: "native", Epoch: "original-host", State: domain.SideCompactionPending, Content: json.RawMessage(`{"draft":"before","attachments":["file:///reference"]}`)}
			if err := s.RegisterSideCompaction(ctx, r); err != nil {
				t.Fatal(err)
			}
			if err := s.SaveSideCompactionContent(ctx, r.SideID, json.RawMessage(`{"draft":"after"}`)); err != nil {
				t.Fatal(err)
			}
			rows, err := s.SideCompactions(ctx, true)
			if err != nil || len(rows) != 1 || string(rows[0].Content) != `{"draft":"after"}` {
				t.Fatalf("content = %+v, %v", rows, err)
			}
			if err := s.TransitionSideCompaction(ctx, r.ID, "replacement-host", state, false, false); !errors.Is(err, domain.ErrSideCompactionTransition) {
				t.Fatalf("replacement host changed result: %v", err)
			}
			historical := state == domain.SideCompactionUnknown
			if err := s.TransitionSideCompaction(ctx, r.ID, r.Epoch, state, historical, false); err != nil {
				t.Fatal(err)
			}
			if historical {
				if err := s.TransitionSideCompaction(ctx, r.ID, r.Epoch, domain.SideCompactionSettled, false, false); !errors.Is(err, domain.ErrSideCompactionTransition) {
					t.Fatalf("historical result changed: %v", err)
				}
				if err := s.TransitionSideCompaction(ctx, r.ID, r.Epoch, state, true, true); err != nil {
					t.Fatal(err)
				}
			}
			rows, err = s.SideCompactions(ctx, true)
			if err != nil || len(rows) != 0 {
				t.Fatalf("unresolved = %+v, %v", rows, err)
			}
			rows, err = s.SideCompactions(ctx, false)
			if err != nil || len(rows) != 1 || len(rows[0].Content) != 0 || rows[0].State != state || rows[0].Closed != historical {
				t.Fatalf("metadata = %+v, %v", rows, err)
			}
		})
	}
}
