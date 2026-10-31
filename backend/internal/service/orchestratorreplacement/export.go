package orchestratorreplacement

import (
	"context"
	"errors"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/transcriptexport"
)

var errNoStopClearPending = errors.New("no-stop intent clearance pending")

// export advances one durable boundary at a time. Captured facts survive a new
// coordinator; an operation phase alone never proves that a provider stopped.
func (s *Service) export(ctx context.Context, op domain.OrchestratorReplacement) (domain.OrchestratorReplacement, error) {
	reader := s.deps.NativeTranscripts
	if _, ok := reader.(ports.NativeTranscriptCapturer); !ok {
		return op, apierr.Unavailable("TRANSCRIPT_CAPTURE_UNAVAILABLE", "A native reader with cutoff capture is required")
	}
	if op.FrozenAt == nil || op.Preparation == nil || op.Preparation.VerifiedAt == nil || op.Transcript == nil {
		return op, apierr.Conflict("ORCHESTRATOR_HANDOFF_UNVERIFIED", "Frozen verified preparation is required before export", nil)
	}
	if op.SourceStop == nil {
		if op.SourceStopIntentAt == nil {
			if !s.deps.Now().Before(op.DeadlineAt) {
				return s.wait(ctx, op, domain.ReplacementAwaitingSource, op.Phase, apierr.Conflict("ORCHESTRATOR_HANDOFF_SOURCE_IDLE_TIMEOUT", "Source did not become quiescent before the deadline", nil))
			}
			// Busy is a positive no-stop receipt. Persisted UpdatedAt provides a
			// bounded retry delay across coordinator restart without new snapshots.
			if op.Error != nil && op.Error.Code == "ORCHESTRATOR_HANDOFF_SOURCE_BUSY" && s.deps.Now().Before(op.UpdatedAt.Add(5*time.Second)) {
				return op, nil
			}
			// Recheck DB ownership after preparation and intake freezing: new
			// settled turns may have appeared since the initial preflight.
			snapshot, err := s.deps.Store.LoadTranscriptSourceSnapshot(ctx, op.SourceSessionID)
			if err != nil {
				return op, err
			}
			if err := transcriptexport.VerifyNativeTurnOwnership(snapshot); err != nil {
				return op, err
			}
			now := s.deps.Now().UTC()
			op.SourceStopIntentAt = &now
			return s.save(ctx, op)
		}
		stop, err := s.deps.Runtime.QuiesceReplacementSource(ctx, op)
		if errors.Is(err, domain.ErrReplacementSourceBusy) {
			observed, observeErr := s.deps.Runtime.ObserveReplacementSource(ctx, op)
			if observeErr != nil || observed.Owner != op.SourceOwner || observed.NativeID != sourceNativeID(op.SourceOwner) || observed.Provenance != domain.NativeThreadMain || observed.ObservedAt.IsZero() || (observed.State != ports.ChatControllerReady && observed.State != ports.ChatControllerBusy) {
				return op, stopUncertain()
			}
			next, cleared, clearErr := s.deps.Store.ClearReplacementStopIntent(ctx, op.ID, op.Revision, observed, s.deps.Now().UTC())
			if clearErr != nil {
				return op, errors.Join(errNoStopClearPending, clearErr)
			}
			if !cleared {
				return op, errRevisionChanged
			}
			return next, nil
		}
		if err != nil {
			return op, stopUncertain()
		}
		source, found, err := s.deps.Store.GetSession(ctx, op.SourceSessionID)
		if err != nil {
			return op, err
		}
		if !found || stop.Owner != op.SourceOwner || stop.CapabilityVerifier != op.SourceCapabilityVerifier || !stop.MatchesSource(source) {
			return op, stopUncertain()
		}
		op.SourceStop = &stop
		op.Error, op.ResumePhase = nil, ""
		return s.save(ctx, op)
	}
	snapshot, err := s.deps.Store.LoadTranscriptSourceSnapshot(ctx, op.SourceSessionID)
	if err != nil {
		return op, err
	}
	if !op.SourceStop.MatchesSource(snapshot.Session) {
		return op, stopUncertain()
	}
	if op.Transcript.CapturedAt == nil {
		cuts, err := transcriptexport.CaptureBoundaries(ctx, snapshot, reader)
		if err != nil {
			return op, err
		}
		now := s.deps.Now().UTC()
		op.Transcript.CapturedAt, op.Transcript.ExportedAt = &now, &now
		op.Transcript.NativeCutoffs, op.Transcript.SnapshotFingerprint = cuts, snapshot.Fingerprint
		return s.save(ctx, op)
	}
	if snapshot.Fingerprint != op.Transcript.SnapshotFingerprint {
		return op, apierr.Conflict("HANDOFF_EVIDENCE_SOURCE_CHANGED", "Captured AO source facts changed before export", nil)
	}
	out, err := transcriptexport.Build(ctx, snapshot, op.Transcript.NativeCutoffs, reader)
	if err != nil {
		return op, err
	}
	return s.deps.Documents.PublishReplacementEvidence(ctx, op.ID, op.Revision, out)
}

func stopUncertain() error {
	return apierr.Conflict("ORCHESTRATOR_HANDOFF_SOURCE_STOP_UNCERTAIN", "Source shutdown cannot be positively confirmed; recovery must verify the same owner", nil)
}
