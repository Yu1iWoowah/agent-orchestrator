package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/gob"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

const (
	maxReplacementRepairBytes        = 16 << 20
	maxReplacementRepairSnapshots    = 1024
	maxReplacementRepairHistoryBytes = 512 << 20
	// Keep a complete bounded lifecycle tail available after ordinary history
	// fills up. Reserve bytes for maximum-size capsules, not average row sizes.
	reservedReplacementRepairSnapshots = 64
	reservedReplacementRepairBytes     = reservedReplacementRepairSnapshots * maxReplacementRepairBytes
)

type replacementRecoveryCheckpointKey struct{}

func replacementRecoveryCheckpointContext(ctx context.Context, q *gen.Queries) context.Context {
	return context.WithValue(ctx, replacementRecoveryCheckpointKey{}, q)
}

// Existing fact/phase validators still authorize every change before this
// classification. Metadata churn must not spend the provider recovery reserve.
func replacementRecoveryProgress(before, after domain.OrchestratorReplacement) bool {
	if before.Phase.Terminal() || before.SourceStopIntentAt == nil && after.SourceStopIntentAt != nil {
		return false
	}
	if after.Phase == domain.ReplacementCancelled {
		return true
	}
	if before.SourceStopIntentAt == nil && before.SourceStop == nil && before.SourceRetiredAt == nil {
		return false
	}
	before.Revision, after.Revision, before.IntakeRevision, after.IntakeRevision = 0, 0, 0, 0
	before.UpdatedAt, after.UpdatedAt, before.DeadlineAt, after.DeadlineAt = time.Time{}, time.Time{}, time.Time{}, time.Time{}
	before.Error, after.Error, before.ResumePhase, after.ResumePhase = nil, nil, "", ""
	return !reflect.DeepEqual(before, after)
}

// Gob preserves raw string bytes: JSON would replace invalid UTF-8 and could
// give distinct damaged rows the same apparent CAS digest.
func replacementRawDigest(row gen.OrchestratorReplacement) (string, error) {
	h := sha256.New()
	if err := gob.NewEncoder(h).Encode(row); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func repairPayloadDigest(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func checkpointRow(snapshot gen.OrchestratorReplacementRepairSnapshot) (gen.OrchestratorReplacement, error) {
	var row gen.OrchestratorReplacement
	if len(snapshot.Payload) > maxReplacementRepairBytes || repairPayloadDigest(snapshot.Payload) != snapshot.Digest {
		return row, domain.ErrReplacementRepairUnverified
	}
	if err := json.Unmarshal(snapshot.Payload, &row); err != nil {
		return row, domain.ErrReplacementRepairUnverified
	}
	if row.ID != snapshot.OperationID || row.Revision != snapshot.Revision {
		return row, domain.ErrReplacementRepairUnverified
	}
	if _, err := decodeReplacement(row); err != nil {
		return row, domain.ErrReplacementRepairUnverified
	}
	return row, nil
}

func checkpointMatches(row, sealed gen.OrchestratorReplacement) bool {
	if row.IntakeRevision < sealed.IntakeRevision {
		return false
	}
	// Canonical intake triggers may increment this without an operation save.
	row.IntakeRevision = sealed.IntakeRevision
	return row == sealed
}

func (s *Store) saveReplacementCheckpoint(ctx context.Context, q *gen.Queries, id string) error {
	row, err := q.GetOrchestratorReplacement(ctx, id)
	if err != nil {
		return err
	}
	operation, err := decodeReplacement(row)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(row)
	if err != nil {
		return err
	}
	if len(payload) > maxReplacementRepairBytes {
		return domain.ErrReplacementRepairCapacity
	}
	if operation.Phase != domain.ReplacementCancelled && operation.SourceStopIntentAt == nil && operation.SourceStop == nil && operation.SourceRetiredAt == nil {
		// A bounded capsule must leave room for the attempt archive made by
		// Cancel, not only for today's row. Use the same transformation as
		// Reset and maximum-width timestamps/revisions; this projection is
		// never published and grants no cancellation authority.
		at := time.Date(9999, time.December, 31, 23, 59, 59, 999999999, time.UTC)
		cancelled := archiveReplacementAttempt(operation, "00000000-0000-0000-0000-000000000000", at)
		cancelled.Phase = domain.ReplacementCancelled
		cancelled.Revision, cancelled.IntakeRevision = math.MaxInt64, math.MaxInt64
		state, err := json.Marshal(cancelled)
		if err != nil {
			return err
		}
		projection := row
		projection.StateJson, projection.Phase, projection.IsActive, projection.UpdatedAt = string(state), string(cancelled.Phase), false, at.Format(time.RFC3339Nano)
		projection.Revision, projection.IntakeRevision = math.MaxInt64, math.MaxInt64
		projected, err := json.Marshal(projection)
		if err != nil {
			return err
		}
		if len(projected) > maxReplacementRepairBytes {
			return domain.ErrReplacementRepairCapacity
		}
	}
	usage, err := q.GetReplacementRepairSnapshotUsage(ctx, id)
	if err != nil {
		return err
	}
	// New stop intents always use the ordinary budget. Once admitted, the
	// clearance/stop receipt and bounded lifecycle can use the reserved tail.
	countLimit := int64(maxReplacementRepairSnapshots)
	byteLimit := int64(maxReplacementRepairHistoryBytes)
	if ctx.Value(replacementRecoveryCheckpointKey{}) == q {
		countLimit += reservedReplacementRepairSnapshots
		byteLimit += reservedReplacementRepairBytes
	}
	// Immutable history is never pruned to recover capacity.
	if usage.Snapshots >= countLimit || usage.PayloadBytes > byteLimit-int64(len(payload)) {
		return domain.ErrReplacementRepairCapacity
	}
	return q.InsertReplacementRepairSnapshot(ctx, gen.InsertReplacementRepairSnapshotParams{OperationID: id, Revision: row.Revision, Digest: repairPayloadDigest(payload), Payload: payload})
}

func legacyReplacementHasNoProof(op domain.OrchestratorReplacement) bool {
	return op.FrozenAt == nil && op.Transcript == nil && op.SourceStopIntentAt == nil && op.SourceStop == nil && op.SourceRetiredAt == nil &&
		op.WriterActivation == nil && op.SuccessorSessionID == "" && op.SuccessorController == nil && op.Bootstrap == nil && op.ResumeEvidence == nil &&
		op.SpawnRequestID == "" && op.SpawnRequestHash == "" && op.SpawnRetryGeneration == 0 && len(op.SpawnAttempts) == 0 &&
		op.BootstrapClientID == "" && op.ResumeProbeClientID == "" && op.IntakeReleasedAt == nil && op.IntakeDrainRequestedAt == nil && op.IntakeTransfer == nil &&
		op.SourceIntakeReleasedAt == nil && len(op.Attempts) == 0 && len(op.Notifications) == 0 && len(op.Repairs) == 0 &&
		(op.Preparation == nil || op.Preparation.VerifiedAt == nil && op.Preparation.WriteReceiptID == "")
}

func (s *Store) decodeCheckpointedReplacement(ctx context.Context, q *gen.Queries, row gen.OrchestratorReplacement) (domain.OrchestratorReplacement, error) {
	op, err := decodeReplacement(row)
	if err != nil {
		return op, err
	}
	latest, err := q.GetLatestReplacementRepairSnapshot(ctx, row.ID)
	if errors.Is(err, sql.ErrNoRows) {
		if legacyReplacementHasNoProof(op) {
			return op, nil
		}
		return op, domain.ErrReplacementRepairUnverified
	}
	if err != nil {
		return op, err
	}
	sealed, err := checkpointRow(latest)
	if err != nil || !checkpointMatches(row, sealed) {
		return op, domain.ErrReplacementRepairUnverified
	}
	return op, nil
}

// Read both row and checkpoint in one DB snapshot. A concurrently committed
// revision must not make an otherwise valid read look like data corruption.
func (s *Store) replacementRead(ctx context.Context, read func(context.Context, *gen.Queries) error) error {
	if q := s.replacementReader(ctx); q != s.qr {
		return read(ctx, q)
	}
	tx, err := s.readDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	q := gen.New(tx)
	return read(context.WithValue(ctx, replacementTransactionKey{}, q), q)
}

func (s *Store) readReplacement(ctx context.Context, query func(context.Context, *gen.Queries) (gen.OrchestratorReplacement, error)) (domain.OrchestratorReplacement, bool, error) {
	var op domain.OrchestratorReplacement
	found := false
	err := s.replacementRead(ctx, func(tx context.Context, q *gen.Queries) error {
		row, err := query(tx, q)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		op, err = s.decodeCheckpointedReplacement(tx, q, row)
		found = err == nil
		return err
	})
	return op, found, err
}

func validateReplacementRepairs(op domain.OrchestratorReplacement) error {
	var last int64
	for _, r := range op.Repairs {
		if r.BeforeRevision <= 0 || r.SnapshotRevision <= 0 || r.AfterRevision <= r.BeforeRevision || r.AfterRevision <= r.SnapshotRevision || r.AfterRevision <= last || !replacementHash(r.BeforeDigest) || !replacementHash(r.SnapshotDigest) || r.RepairedAt.IsZero() {
			return domain.ErrReplacementStateInvalid
		}
		last = r.AfterRevision
	}
	return nil
}

func preserveReplacementRepairs(before, after domain.OrchestratorReplacement) error {
	if !reflect.DeepEqual(before.Repairs, after.Repairs) {
		return domain.ErrReplacementStateInvalid
	}
	return nil
}

func repairSnapshotDomain(row gen.OrchestratorReplacement, snap gen.OrchestratorReplacementRepairSnapshot) domain.ReplacementRepairSnapshot {
	return domain.ReplacementRepairSnapshot{OperationID: row.ID, ProjectID: domain.ProjectID(row.ProjectID), SourceSessionID: domain.SessionID(row.SourceSessionID), Revision: snap.Revision, Digest: snap.Digest, Payload: bytes.Clone(snap.Payload)}
}
