package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"math"
	"time"
	"unicode/utf8"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// DiagnoseOrchestratorReplacement reads raw CAS identity without changing the fence.
func (s *Store) DiagnoseOrchestratorReplacement(ctx context.Context, id string) (domain.ReplacementDiagnostic, bool, error) {
	var out domain.ReplacementDiagnostic
	found := false
	err := s.replacementRead(ctx, func(tx context.Context, q *gen.Queries) error {
		row, err := q.GetOrchestratorReplacement(tx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		digest, err := replacementRawDigest(row)
		if err != nil {
			return err
		}
		out = domain.ReplacementDiagnostic{OperationID: row.ID, ProjectID: domain.ProjectID(row.ProjectID), SourceSessionID: domain.SessionID(row.SourceSessionID), Revision: row.Revision, Active: row.IsActive, RawDigest: digest}
		_, readErr := s.decodeCheckpointedReplacement(tx, q, row)
		out.Corrupt = readErr != nil
		snapshot, err := q.GetLatestReplacementRepairSnapshot(tx, id)
		if err == nil {
			out.SnapshotRevision, out.SnapshotDigest = snapshot.Revision, snapshot.Digest
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		found = true
		return nil
	})
	return out, found, err
}

// GetReplacementRepairSnapshot returns only the latest store-sealed row.
func (s *Store) GetReplacementRepairSnapshot(ctx context.Context, id string) (domain.ReplacementRepairSnapshot, bool, error) {
	var out domain.ReplacementRepairSnapshot
	found := false
	err := s.replacementRead(ctx, func(tx context.Context, q *gen.Queries) error {
		snap, err := q.GetLatestReplacementRepairSnapshot(tx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		row, err := checkpointRow(snap)
		if err != nil {
			return err
		}
		out = repairSnapshotDomain(row, snap)
		found = true
		return nil
	})
	return out, found, err
}

// RepairOrchestratorReplacement restores only a sealed snapshot. No lease, session, queue or native
// controller is modified; recovery remains fenced until a separate safe action.
func (s *Store) RepairOrchestratorReplacement(ctx context.Context, in domain.ReplacementRepairRequest, at time.Time) (domain.OrchestratorReplacement, bool, error) {
	var result domain.OrchestratorReplacement
	applied := false
	if at.IsZero() || !replacementHash(in.ExpectedDigest) || in.Snapshot.OperationID != in.OperationID || !replacementHash(in.Snapshot.Digest) || len(in.Snapshot.Payload) > maxReplacementRepairBytes {
		return result, false, domain.ErrReplacementRepairUnverified
	}
	err := s.replacementTx(ctx, func(tx context.Context, q *gen.Queries) error {
		current, err := q.GetOrchestratorReplacement(tx, in.OperationID)
		if err != nil {
			return err
		}
		latest, err := q.GetLatestReplacementRepairSnapshot(tx, in.OperationID)
		if errors.Is(err, sql.ErrNoRows) {
			return domain.ErrReplacementRepairUnverified
		}
		if err != nil {
			return err
		}
		requested, err := q.GetReplacementRepairSnapshot(tx, gen.GetReplacementRepairSnapshotParams{OperationID: in.OperationID, Revision: in.Snapshot.Revision})
		if errors.Is(err, sql.ErrNoRows) {
			return domain.ErrReplacementRepairUnverified
		}
		if err != nil {
			return err
		}
		sealed, err := checkpointRow(requested)
		if err != nil {
			return err
		}
		if in.Snapshot.Digest != requested.Digest || !bytes.Equal(in.Snapshot.Payload, requested.Payload) || in.Snapshot.ProjectID != domain.ProjectID(sealed.ProjectID) || in.Snapshot.SourceSessionID != domain.SessionID(sealed.SourceSessionID) {
			return domain.ErrReplacementRepairUnverified
		}
		// Lost response: return the same committed repair without minting another.
		if now, readErr := s.decodeCheckpointedReplacement(tx, q, current); readErr == nil {
			for _, r := range now.Repairs {
				if r.BeforeRevision == in.ExpectedRevision && r.BeforeDigest == in.ExpectedDigest && r.SnapshotRevision == in.Snapshot.Revision && r.SnapshotDigest == in.Snapshot.Digest {
					result = now
					return nil
				}
			}
		}
		if latest.Revision != requested.Revision || latest.Digest != requested.Digest {
			return domain.ErrReplacementRepairUnverified
		}
		digest, err := replacementRawDigest(current)
		if err != nil {
			return err
		}
		if current.Revision != in.ExpectedRevision || digest != in.ExpectedDigest {
			return domain.ErrReplacementRepairConflict
		}
		if !current.IsActive || !sealed.IsActive || current.Revision == math.MaxInt64 || sealed.Revision == math.MaxInt64 || len(current.StateJson) > maxReplacementRepairBytes {
			return domain.ErrReplacementRepairUnsafe
		}
		if err := preserveRawReplacementFacts(current.StateJson, sealed.StateJson); err != nil {
			return err
		}
		op, err := decodeReplacement(sealed)
		if err != nil {
			return err
		}
		next := max(current.Revision, sealed.Revision) + 1
		preStop := op.SourceStopIntentAt == nil && op.SourceStop == nil && op.SourceRetiredAt == nil && op.SuccessorSessionID == "" && op.WriterActivation == nil
		op.Repairs = append(op.Repairs, domain.ReplacementRepairReceipt{BeforeRevision: current.Revision, BeforeDigest: digest, SnapshotRevision: sealed.Revision, SnapshotDigest: requested.Digest, AfterRevision: next, PreStopSnapshot: preStop, RepairedAt: at.UTC()})
		op.Phase, op.ResumePhase = domain.ReplacementRecoveryRequired, ""
		op.Error = &domain.ReplacementFailure{Code: "ORCHESTRATOR_REPLACEMENT_OPERATOR_REPAIRED", Message: "Trusted data restored; verify recovery before any provider action", Retryable: false}
		op.Revision, op.IntakeRevision = next, max(current.IntakeRevision, sealed.IntakeRevision)
		op.UpdatedAt = at.UTC()
		if err := validateReplacement(op); err != nil {
			return err
		}
		state, err := json.Marshal(op)
		if err != nil {
			return err
		}
		count, err := q.RestoreReplacementFromRepairSnapshot(tx, gen.RestoreReplacementFromRepairSnapshotParams{ID: in.OperationID, ExpectedRevision: current.Revision, ProjectID: sealed.ProjectID, SourceSessionID: sealed.SourceSessionID, OperationKey: sealed.OperationKey, RequestHash: sealed.RequestHash, NewRevision: next, IntakeRevision: op.IntakeRevision, StateJson: string(state), CreatedAt: sealed.CreatedAt, UpdatedAt: at.UTC().Format(time.RFC3339Nano)})
		if err != nil {
			return err
		}
		if count != 1 {
			return domain.ErrReplacementRepairConflict
		}
		if err := s.saveReplacementCheckpoint(tx, q, op.ID); err != nil {
			return err
		}
		result, applied = op, true
		return nil
	})
	return result, applied && err == nil, err
}

// Unknown positive side effects cannot be discarded by restoring an older view.
// Missing fields may be recovered from a trusted checkpoint; present, different
// native/managed facts require a separate recovery procedure.
func preserveRawReplacementFacts(current, known string) error {
	if !utf8.ValidString(current) || !replacementJSONUnique([]byte(current)) {
		return domain.ErrReplacementRepairUnsafe
	}
	var raw, snapshot map[string]json.RawMessage
	if json.Unmarshal([]byte(current), &raw) != nil || json.Unmarshal([]byte(known), &snapshot) != nil {
		return domain.ErrReplacementRepairUnsafe
	}
	// Only the request/status envelope may be repaired. Every present fact,
	// including future or unknown fields, must match the sealed record exactly.
	for key, a := range raw {
		switch key {
		case "schemaVersion", "id", "projectId", "sourceSessionId", "operationKey", "requestHash", "phase", "resumePhase", "error", "projectFingerprint", "createdAt", "updatedAt", "deadlineAt":
			continue
		}
		if len(a) == 0 || bytes.Equal(bytes.TrimSpace(a), []byte("null")) {
			continue
		}
		if !repairJSONEqual(a, snapshot[key]) {
			return domain.ErrReplacementRepairUnsafe
		}
	}
	return nil
}

func repairJSONEqual(a, b json.RawMessage) bool {
	var av, bv any
	da, db := json.NewDecoder(bytes.NewReader(a)), json.NewDecoder(bytes.NewReader(b))
	da.UseNumber()
	db.UseNumber()
	if da.Decode(&av) != nil || db.Decode(&bv) != nil {
		return false
	}
	ab, ae := json.Marshal(av)
	bb, be := json.Marshal(bv)
	return ae == nil && be == nil && bytes.Equal(ab, bb)
}

// SQLite selects the first duplicate key while encoding/json selects the last.
// Reject ambiguity before comparing facts or trusting a legacy row.
func replacementJSONUnique(data []byte) bool {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if !replacementJSONValueUnique(dec, 0) {
		return false
	}
	_, err := dec.Token()
	return errors.Is(err, io.EOF)
}

func replacementJSONValueUnique(dec *json.Decoder, depth int) bool {
	if depth > 128 {
		return false
	}
	token, err := dec.Token()
	if err != nil {
		return false
	}
	delimiter, structured := token.(json.Delim)
	if !structured {
		return true
	}
	switch delimiter {
	case '{':
		seen := map[string]bool{}
		for dec.More() {
			key, err := dec.Token()
			if err != nil {
				return false
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return false
			}
			seen[name] = true
			if !replacementJSONValueUnique(dec, depth+1) {
				return false
			}
		}
		end, err := dec.Token()
		return err == nil && end == json.Delim('}')
	case '[':
		for dec.More() {
			if !replacementJSONValueUnique(dec, depth+1) {
				return false
			}
		}
		end, err := dec.Token()
		return err == nil && end == json.Delim(']')
	default:
		return false
	}
}
