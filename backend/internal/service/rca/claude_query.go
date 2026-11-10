package rca

import (
	"context"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/components/hcg/policy"
	rcapolicy "github.com/aoagents/agent-orchestrator/components/hcg/rca"
)

func (s *Service) getClaude(ctx context.Context, rec domain.SessionRecord, inputs policy.Inputs, q domain.RCAQuery, key rcapolicy.Key, receipt rcapolicy.Revision) (domain.RCAComposition, error) {
	out := domain.RCAComposition{ProjectID: rec.ProjectID, SessionID: rec.ID, Status: "unavailable", Reason: "waiting_safe_launch", RequestComposition: "UNKNOWN", Baseline: baseline(approvedBaseline)}
	page, err := s.events.Events(ctx, rec.ID, q.AfterID, q.ThroughID, q.Limit)
	if err != nil {
		return out, err
	}
	out.Canonical = canonicalProjection(page)
	snapshot := func(value *domain.RCAClaudeInline, why string) domain.RCAComposition {
		view, viewWhy := s.core.CachedView(ctx, key, inputs)
		out.Policy = policyView(view)
		s.mu.Lock()
		capture := s.captureSessions[rec.ID]
		reason := s.claudeReason[rec.ID]
		failed, closed := s.failed, s.closed
		s.mu.Unlock()
		current := capture != nil && capture.owner == stamp(rec) && s.core.Current(capture.receipt)
		if capture != nil && !current {
			s.core.Invalidate(capture.key)
			s.revokeClaude(rec.ID, "source_stopped")
			capture = nil
		}
		if value == nil || current && value.CaptureID != capture.id {
			value = &domain.RCAClaudeInline{ProjectID: rec.ProjectID, SessionID: rec.ID, Role: string(rec.Kind), Coverage: "observed_inline_attempt", Method: "utf8_bytes_div4_v1", Attempts: []domain.RCAClaudeAttempt{}}
		}
		if current && value.CaptureID == "" {
			value.CaptureID = capture.id
		}
		value.Requested = view.EffectiveEnabled
		value.Active = current && value.CaptureID == capture.id && view.EffectiveEnabled && !failed && !closed
		switch {
		case !view.EffectiveEnabled:
			value.Reason = "disabled"
			value.Attempts = nil
		case failed || closed:
			value.Reason = "analysis_failed"
			value.Attempts = nil
		case why != "":
			value.Reason = why
			value.Attempts = nil
		case viewWhy != "":
			value.Reason = viewWhy
			value.Attempts = nil
		case !s.core.Current(receipt):
			value.Reason = "policy_changed"
			value.Active = false
			value.Attempts = nil
		case reason != "":
			value.Reason = reason
		case value.Active:
			value.Reason = ""
		case value.CaptureID != "":
			value.Reason = "source_stopped"
		default:
			value.Reason = "waiting_safe_launch"
		}
		if value.Attempts == nil {
			value.Attempts = []domain.RCAClaudeAttempt{}
		}
		out.ClaudeInline = value
		out.Reason = value.Reason
		if len(value.Attempts) > 0 {
			out.Status = "partial"
		} else {
			out.Status = "unavailable"
		}
		return out
	}
	if !s.queryBusy.CompareAndSwap(false, true) {
		return snapshot(nil, "writer_busy"), nil
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		s.queryBusy.Store(false)
		return snapshot(nil, "canceled"), nil
	}
	s.wg.Add(1)
	s.mu.Unlock()
	type result struct {
		value *domain.RCAClaudeInline
		why   string
	}
	done := make(chan result, 1)
	readCtx, cancel := context.WithCancel(s.ctx)
	stop := context.AfterFunc(ctx, cancel)
	go func() {
		value := result{}
		defer func() {
			if recover() != nil {
				s.core.Unavailable()
				value = result{why: "analysis_failed"}
			}
			stop()
			cancel()
			s.queryBusy.Store(false)
			s.wg.Done()
			done <- value
		}()
		view, why := s.core.View(readCtx, key, inputs)
		if why != "" {
			value.why = why
			return
		}
		if !view.EffectiveEnabled {
			return
		}
		value.why = s.core.WithStorage(readCtx, func(ctx context.Context) error {
			row, ok, err := s.projection.LoadClaudeInline(ctx, rec.ProjectID, rec.ID)
			if err != nil {
				return err
			}
			if ok {
				value.value = &row
			}
			return s.projection.Ready(ctx)
		})
		if value.why == "storage_unavailable" {
			s.core.Unavailable()
			value.why = s.storageReason()
		}
	}()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case value := <-done:
		return snapshot(value.value, value.why), nil
	case <-ctx.Done():
		cancel()
		return snapshot(nil, "canceled"), nil
	case <-timer.C:
		cancel()
		return snapshot(nil, "writer_busy"), nil
	}
}
