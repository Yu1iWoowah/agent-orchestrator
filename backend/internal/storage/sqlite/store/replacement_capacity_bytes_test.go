package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

func TestReplacementByteCapacityPreservesSuccessorBinding(t *testing.T) {
	// Already-produced stop/retirement facts use the existing privileged
	// fixture; every history capsule below is a normal metadata-only save.
	f := newCanonicalIntakeFixture(t, false, false)
	ctx := context.Background()
	op := f.op
	op.Error = &domain.ReplacementFailure{Code: "QUOTA_FIXTURE"}
	if saved, err := f.st.SaveOrchestratorReplacement(ctx, op); err != nil || !saved {
		t.Fatal(saved, err)
	}
	const ordinaryBytes = 512 << 20
	var used int64
	for i := 0; ; i++ {
		var err error
		op, _, err = f.st.GetOrchestratorReplacement(ctx, op.ID)
		if err != nil {
			t.Fatal(err)
		}
		var latest []byte
		if err := f.raw.QueryRow(`SELECT payload FROM orchestrator_replacement_repair_snapshots WHERE operation_id=? ORDER BY revision DESC LIMIT 1`, op.ID).Scan(&latest); err != nil {
			t.Fatal(err)
		}
		if err := f.raw.QueryRow(`SELECT sum(length(payload)) FROM orchestrator_replacement_repair_snapshots WHERE operation_id=?`, op.ID).Scan(&used); err != nil {
			t.Fatal(err)
		}
		if used == ordinaryBytes-1 {
			break
		}
		if i >= 40 {
			t.Fatal("byte fixture did not reach the boundary", used)
		}
		// Calculate the next capsule's framing without padding so the last
		// normal save leaves exactly one byte, independent of revision width.
		var next gen.OrchestratorReplacement
		if err := json.Unmarshal(latest, &next); err != nil {
			t.Fatal(err)
		}
		op.Error.Message = ""
		state, err := json.Marshal(op)
		if err != nil {
			t.Fatal(err)
		}
		next.StateJson, next.Revision = string(state), op.Revision+1
		framed, err := json.Marshal(next)
		if err != nil {
			t.Fatal(err)
		}
		framing := len(framed)
		size := min(15<<20, int(ordinaryBytes-used-1))
		if size < framing {
			t.Fatal("insufficient room for final boundary capsule", size, framing)
		}
		op.Error.Message = strings.Repeat("x", size-framing)
		if saved, err := f.st.SaveOrchestratorReplacement(ctx, op); err != nil || !saved {
			t.Fatal("ordinary history save", used, saved, err)
		}
	}
	op.Error.Message = "metadata cannot consume recovery bytes"
	if saved, err := f.st.SaveOrchestratorReplacement(ctx, op); saved || !errors.Is(err, domain.ErrReplacementRepairCapacity) {
		t.Fatal("ordinary metadata escaped the byte limit", saved, err)
	}
	writer := sampleRecord(string(op.ProjectID))
	writer.Kind, writer.Mode = domain.KindOrchestrator, domain.SessionModeChat
	writer.ClientRequestID, writer.ClientRequestHash = op.SpawnRequestID, op.SpawnRequestHash
	created, fresh, err := f.st.CreateClientRequestSession(ctx, writer)
	if err != nil || !fresh {
		t.Fatal("ordinary bytes trapped managed successor binding", fresh, err)
	}
	current, found, err := f.st.GetOrchestratorReplacement(ctx, op.ID)
	if err != nil || !found || current.SuccessorSessionID != created.ID || current.SourceStop == nil || current.SourceRetiredAt == nil {
		t.Fatal("successor binding lost trusted lifecycle facts", found, err)
	}
	again, fresh, err := f.st.CreateClientRequestSession(ctx, writer)
	if err != nil || fresh || again.ID != created.ID {
		t.Fatal("lost response duplicated the reserved successor", fresh, err)
	}
}
