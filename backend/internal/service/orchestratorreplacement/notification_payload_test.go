package orchestratorreplacement_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestReplacementActivationFreezesWorkerFYIPayload(t *testing.T) {
	x := activationCoordinatorFixture(t)
	ctx := context.Background()
	op, err := x.service().Advance(ctx, x.operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(op)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot map[string]json.RawMessage
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Version int    `json:"version"`
		Text    string `json:"text"`
		SHA256  string `json:"sha256"`
	}
	if len(snapshot["workerFYIPayload"]) == 0 {
		t.Fatal("activation did not freeze FYI payload")
	}
	if err := json.Unmarshal(snapshot["workerFYIPayload"], &payload); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(op.WorkerFYI()))
	if payload.Version != 1 || payload.Text != op.WorkerFYI() || payload.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatal("incorrect frozen payload", payload)
	}
	for _, mutation := range []string{"remove", "replace"} {
		copy := map[string]json.RawMessage{}
		for key, value := range snapshot {
			copy[key] = value
		}
		if mutation == "remove" {
			delete(copy, "workerFYIPayload")
		} else {
			copy["workerFYIPayload"] = json.RawMessage(`{"version":1,"text":"forged","sha256":"` + payload.SHA256 + `"}`)
		}
		encoded, err := json.Marshal(copy)
		if err != nil {
			t.Fatal(err)
		}
		var changed domain.OrchestratorReplacement
		if err := json.Unmarshal(encoded, &changed); err != nil {
			t.Fatal(err)
		}
		changed.Revision, changed.IntakeRevision = op.Revision, op.IntakeRevision
		if saved, err := x.f.store.SaveOrchestratorReplacement(ctx, changed); saved || !errors.Is(err, domain.ErrReplacementStateInvalid) {
			t.Fatal("generic save changed payload", mutation, saved, err)
		}
	}
	// Before activation even a valid payload cannot be minted by generic save.
	before := activationCoordinatorFixture(t)
	raw, err = json.Marshal(before.operation)
	if err != nil {
		t.Fatal(err)
	}
	snapshot = map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatal(err)
	}
	snapshot["workerFYIPayload"] = json.RawMessage(`{"version":1,"text":"forged","sha256":"` + payload.SHA256 + `"}`)
	raw, err = json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var changed domain.OrchestratorReplacement
	if err := json.Unmarshal(raw, &changed); err != nil {
		t.Fatal(err)
	}
	changed.Revision, changed.IntakeRevision = before.operation.Revision, before.operation.IntakeRevision
	if saved, err := before.f.store.SaveOrchestratorReplacement(ctx, changed); saved || !errors.Is(err, domain.ErrReplacementStateInvalid) {
		t.Fatal("generic save minted payload before activation", saved, err)
	}
}
