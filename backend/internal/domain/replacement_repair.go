package domain

import (
	"errors"
	"time"
)

var (
	// ErrReplacementRepairUnverified refuses data without a matching sealed snapshot.
	ErrReplacementRepairUnverified = errors.New("replacement repair snapshot is not verified")
	// ErrReplacementRepairConflict rejects a stale raw row CAS.
	ErrReplacementRepairConflict = errors.New("replacement repair row changed")
	// ErrReplacementRepairUnsafe retains uncertain native or managed facts.
	ErrReplacementRepairUnsafe = errors.New("replacement repair would change protected facts")
	// ErrReplacementRepairCapacity leaves the current row and immutable history intact.
	ErrReplacementRepairCapacity = errors.New("replacement repair evidence capacity exceeded")
)

// ReplacementDiagnostic is a read-only view of an undecodable reserved row.
// RawDigest covers its complete row envelope, including unparsed state bytes.
type ReplacementDiagnostic struct {
	OperationID      string    `json:"operationId"`
	ProjectID        ProjectID `json:"projectId"`
	SourceSessionID  SessionID `json:"sourceSessionId"`
	Revision         int64     `json:"revision"`
	Active           bool      `json:"active"`
	RawDigest        string    `json:"rawDigest"`
	Corrupt          bool      `json:"corrupt"`
	CorruptionReason string    `json:"corruptionReason,omitempty"`
	SnapshotRevision int64     `json:"snapshotRevision,omitempty"`
	SnapshotDigest   string    `json:"snapshotDigest,omitempty"`
}

// ReplacementRepairSnapshot contains opaque, store-sealed bytes. Transport as
// base64 preserves the exact payload through JSON clients and pretty printers.
type ReplacementRepairSnapshot struct {
	OperationID     string    `json:"operationId"`
	ProjectID       ProjectID `json:"projectId"`
	SourceSessionID SessionID `json:"sourceSessionId"`
	Revision        int64     `json:"revision"`
	Digest          string    `json:"digest"`
	Payload         []byte    `json:"payload"`
}

// ReplacementRepairRequest authorizes one data restore against exact row CAS facts.
type ReplacementRepairRequest struct {
	OperationID      string
	ExpectedRevision int64
	ExpectedDigest   string
	Snapshot         ReplacementRepairSnapshot
}

// HasRepairReceipt identifies a committed replay, not authority for a new repair.
func (op OrchestratorReplacement) HasRepairReceipt(in ReplacementRepairRequest) bool {
	if in.OperationID != op.ID || in.Snapshot.OperationID != op.ID {
		return false
	}
	for _, r := range op.Repairs {
		if r.BeforeRevision == in.ExpectedRevision && r.BeforeDigest == in.ExpectedDigest && r.SnapshotRevision == in.Snapshot.Revision && r.SnapshotDigest == in.Snapshot.Digest {
			return true
		}
	}
	return false
}

// ReplacementRepairReceipt proves a data-only restore, never a provider action.
type ReplacementRepairReceipt struct {
	BeforeRevision   int64     `json:"beforeRevision"`
	BeforeDigest     string    `json:"beforeDigest"`
	SnapshotRevision int64     `json:"snapshotRevision"`
	SnapshotDigest   string    `json:"snapshotDigest"`
	AfterRevision    int64     `json:"afterRevision"`
	PreStopSnapshot  bool      `json:"preStopSnapshot"`
	RepairedAt       time.Time `json:"repairedAt"`
}
