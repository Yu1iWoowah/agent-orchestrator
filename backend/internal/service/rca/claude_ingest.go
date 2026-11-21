package rca

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/components/hcg/policy"
	rcapolicy "github.com/aoagents/agent-orchestrator/components/hcg/rca"
)

type claudeBodyIndex struct {
	digest    [32]byte
	truncated bool
	buckets   [6]domain.RCABucket
	reasons   []string
	mcp       int64
	used      bool
}
type claudeRequestIndex struct {
	bodyID  string
	ordinal int64
}
type claudeCapture struct {
	marker, id        string
	session           domain.SessionID
	nativeID          string
	owner             ownerStamp
	key               rcapolicy.Key
	inputs            policy.Inputs
	receipt           rcapolicy.Revision
	bodies            map[string]claudeBodyIndex
	requests          map[string]claudeRequestIndex
	pending           map[string]string
	responses         map[string]string
	entries, charge   int
	ordinal           int64
	dropped, unpaired int64
}

func claudeScope(rec domain.SessionRecord) bool {
	return rec.ProjectID == "agent-orchestrator" && rec.Harness == domain.HarnessClaudeCode && rec.Mode == domain.SessionModeChat && (rec.Kind == domain.KindOrchestrator || rec.Kind == domain.KindWorker)
}

// revokeClaude never waits for raw reads, writer or disk. Callers invalidate
// the common revision first; an old marker is never restored by on/resume.
func (s *Service) dropClaude(c *claudeCapture, reason string) {
	delete(s.captures, c.marker)
	delete(s.captureSessions, c.session)
	s.indexBytes -= c.charge
	c.charge, c.entries = 0, 0
	c.bodies = nil
	c.requests = nil
	c.pending = nil
	c.responses = nil
	s.claudeReason[c.session] = reason
}
func (s *Service) revokeClaude(id domain.SessionID, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c := s.captureSessions[id]; c != nil {
		s.dropClaude(c, reason)
	}
	s.claudeReason[id] = reason
}
func (s *Service) revokeAllClaude() {
	s.mu.Lock()
	ids := make([]domain.SessionID, 0, len(s.captureSessions))
	for id := range s.captureSessions {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	for _, id := range ids {
		s.core.Invalidate(rcapolicy.Key{ProjectID: "agent-orchestrator", SessionID: string(id)})
		s.core.Invalidate(rcapolicy.Key{ProjectID: "agent-orchestrator", SessionID: string(id), Role: "orchestrator"})
		s.revokeClaude(id, "source_stopped")
	}
}

// armClaude is called only after a trusted safe-launch capability gate and AO
// owner commit. It warms metadata here; AdmitClaude never loads it from disk.
func (s *Service) armClaude(ctx context.Context, rec domain.SessionRecord) (string, string) {
	return s.armClaudeCandidate(ctx, rec, claudePrepared{marker: uuid.NewString(), id: uuid.NewString(), receipt: s.core.Revision(keyFor(rec))})
}
func (s *Service) armClaudeCandidate(ctx context.Context, rec domain.SessionRecord, prepared claudePrepared) (string, string) {
	if !claudeScope(rec) {
		return "", "scope_not_allowed"
	}
	key := keyFor(rec)
	receipt := prepared.receipt
	actual, inputs, err := s.facts(ctx, rec.ID)
	if err != nil {
		return "", "source_metadata_failed"
	}
	if stamp(actual) != stamp(rec) {
		return "", "policy_changed"
	}
	view, why := s.core.View(ctx, key, inputs)
	if why != "" {
		return "", why
	}
	if !view.EffectiveEnabled {
		return "", "disabled"
	}
	m := rec.Metadata
	if m.ControllerGeneration == "" || m.AgentSettingsLaunchID == "" || m.ProviderConversationID == "" || len(m.ProviderConversationID) > 256 {
		return "", "native_generation_unknown"
	}
	if !s.core.Current(receipt) {
		return "", "policy_changed"
	}
	c := &claudeCapture{marker: prepared.marker, id: prepared.id, session: rec.ID, nativeID: strings.Clone(m.ProviderConversationID), owner: stamp(rec), key: key, inputs: inputs, receipt: receipt, bodies: map[string]claudeBodyIndex{}, requests: map[string]claudeRequestIndex{}, pending: map[string]string{}, responses: map[string]string{}}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.failed || s.captureSessions[rec.ID] != nil || !s.core.Current(receipt) {
		return "", "policy_changed"
	}
	if s.indexBytes+1024 > claudeIndexBytes {
		return "", "capture_limit"
	}
	if prepared.generation != "" {
		pending, ok := s.pendingClaude[rec.ID]
		if !ok || pending.id != prepared.id || pending.generation != prepared.generation {
			return "", "source_stopped"
		}
		delete(s.pendingClaude, rec.ID)
	}
	c.charge = 1024
	s.indexBytes += 1024
	s.captures[c.marker], s.captureSessions[rec.ID] = c, c
	s.claudeReason[rec.ID] = ""
	return c.marker, ""
}

type claudeAdmission struct {
	service     *Service
	capture     *claudeCapture
	ticket      *rcapolicy.Ticket
	mu          sync.Mutex
	transferred bool
	released    bool
}

func (a *claudeAdmission) Context() context.Context { return a.ticket.Context() }
func (a *claudeAdmission) Release() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.released && !a.transferred {
		a.service.core.Finish(a.ticket)
	}
	a.released = true
}

// AdmitClaude uses only warmed memory. Marker is a namespace within the
// existing local-process trust boundary, never a bearer or payload authority.
func (s *Service) AdmitClaude(ctx context.Context, marker string) (ports.RCAInlineAdmission, string) {
	if len(marker) != 36 {
		return nil, "source_stopped"
	}
	s.mu.Lock()
	c := s.captures[marker]
	closed := s.closed || s.failed
	s.mu.Unlock()
	if c == nil || closed {
		return nil, "source_stopped"
	}
	if ctx.Err() != nil {
		return nil, "canceled"
	}
	ticket, _, why := s.core.BeginCached(s.ctx, c.key, c.receipt)
	if why == "" {
		why = s.core.Reserve(ticket)
	}
	if why != "" {
		if ticket != nil {
			s.core.Finish(ticket)
		}
		if why == "writer_busy" || why == "analysis_running" {
			s.mu.Lock()
			if s.captures[marker] == c {
				c.dropped++
			}
			s.mu.Unlock()
			why = "writer_busy"
		}
		return nil, why
	}
	s.mu.Lock()
	current := s.captures[marker] == c
	s.mu.Unlock()
	if !current || !s.core.Current(c.receipt) {
		s.core.Finish(ticket)
		return nil, "source_stopped"
	}
	return &claudeAdmission{service: s, capture: c, ticket: ticket}, ""
}

// RejectClaude accepts only fixed controller/transport reason codes; neither a
// request error nor a panic value is formatted or saved.
func (s *Service) RejectClaude(marker, reason string) {
	if !safeReason(reason) {
		reason = "invalid_otlp"
	}
	s.mu.Lock()
	c := s.captures[marker]
	s.mu.Unlock()
	if c != nil {
		s.core.Invalidate(c.key)
		s.revokeClaude(c.session, reason)
	}
}

func (s *Service) indexClaude(c *claudeCapture, keys ...string) bool {
	// Called under s.mu. Charge both payload and conservative container overhead;
	// no raw field or slice can be retained by an index entry.
	charge := 1024
	for _, key := range keys {
		if len(key) > 256 {
			return false
		}
		charge += len(key)
	}
	if c.entries >= claudeIndexEntries || s.indexBytes+charge > claudeIndexBytes {
		return false
	}
	c.entries++
	c.charge += charge
	s.indexBytes += charge
	return true
}
func (a *claudeAdmission) event(event claudeEvent, delta *[]domain.RCAClaudeAttempt) string {
	s, c := a.service, a.capture
	if a.Context().Err() != nil {
		return "canceled"
	}
	// A child/native identity not bound by AO is explicitly excluded. query_source
	// and any self-reported project/role/account are not authority to admit it.
	if event.session != c.nativeID || event.unprovedChild {
		s.mu.Lock()
		if s.captures[c.marker] == c {
			c.unpaired++
		}
		s.mu.Unlock()
		return ""
	}
	var body claudeBodyIndex
	if event.name == "api_request_body" {
		if event.bodyID == "" || len(event.body) == 0 {
			return "unknown_schema"
		}
		r, mcp, why := classifyClaudeBody(event.body, event.truncated)
		if why != "" {
			return why
		}
		body = claudeBodyIndex{digest: sha256.Sum256(event.body), truncated: event.truncated, buckets: r.Buckets, reasons: r.Reasons, mcp: mcp}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.captures[c.marker] != c {
		return "policy_changed"
	}
	addAttempt := func(requestID, bodyID string) string {
		if requestID == "" || bodyID == "" {
			return ""
		}
		if previous, ok := c.requests[requestID]; ok {
			if previous.bodyID != bodyID {
				return "identity_conflict"
			}
			return ""
		}
		observed, ok := c.bodies[bodyID]
		if !ok {
			if previous, ok := c.pending[requestID]; ok {
				if previous != bodyID {
					return "identity_conflict"
				}
				return ""
			}
			if !s.indexClaude(c, requestID, bodyID) {
				return "capture_limit"
			}
			c.pending[strings.Clone(requestID)] = strings.Clone(bodyID)
			return ""
		}
		if (len(*delta)+1)*6 > 512 {
			return "projection_limit"
		}
		if !s.indexClaude(c, requestID, bodyID) {
			return "capture_limit"
		}
		delete(c.pending, requestID)
		observed.used = true
		c.bodies[bodyID] = observed
		c.ordinal++
		c.requests[strings.Clone(requestID)] = claudeRequestIndex{bodyID: strings.Clone(bodyID), ordinal: c.ordinal}
		var mcp *int64
		if observed.buckets[1].Bytes != nil {
			value := observed.mcp
			mcp = &value
		}
		*delta = append(*delta, domain.RCAClaudeAttempt{Ordinal: c.ordinal, Buckets: observed.buckets, MCPBytes: mcp, Reasons: append([]string{"unmatched_attempt"}, observed.reasons...)})
		return ""
	}
	switch event.name {
	case "api_request_body":
		if previous, ok := c.bodies[event.bodyID]; ok {
			if previous.digest != body.digest || previous.mcp != body.mcp || previous.truncated != body.truncated {
				return "identity_conflict"
			}
		} else {
			if !s.indexClaude(c, event.bodyID) {
				return "capture_limit"
			}
			c.bodies[strings.Clone(event.bodyID)] = body
		}
		if why := addAttempt(event.requestID, event.bodyID); why != "" {
			return why
		}
		// Bounded map; no body pointer is held while matching metadata-first events.
		for requestID, bodyID := range c.pending {
			if bodyID == event.bodyID {
				if why := addAttempt(requestID, bodyID); why != "" {
					return why
				}
			}
		}
	case "api_request":
		return addAttempt(event.requestID, event.bodyID)
	case "api_response_body":
		if event.messageID == "" || event.requestID == "" {
			c.unpaired++
			return ""
		}
		if previous, ok := c.responses[event.messageID]; ok {
			if previous != event.requestID {
				return "identity_conflict"
			}
			return ""
		}
		if !s.indexClaude(c, event.messageID, event.requestID) {
			return "capture_limit"
		}
		c.responses[strings.Clone(event.messageID)] = strings.Clone(event.requestID)
		// No exact existing canonical key chain is proven for this schema. Do not
		// guess an event reference from tokens, timestamps or latest canonical row.
	case "subagent_completed":
		c.unpaired++
	case "api_error", "tool_result", "user_prompt":
		// These records are not request-body sources and cannot add input bytes.
	default:
		c.unpaired++
	}
	return ""
}

func (a *claudeAdmission) Read(reader io.Reader, length int64) string {
	var delta []domain.RCAClaudeAttempt
	why := readClaudeOTLP(a.Context(), reader, length, func(e claudeEvent) string { return a.event(e, &delta) })
	if why != "" {
		a.service.RejectClaude(a.capture.marker, why)
		return why
	}
	s, c := a.service, a.capture
	s.mu.Lock()
	if s.closed || s.failed || s.captures[c.marker] != c {
		s.mu.Unlock()
		return "policy_changed"
	}
	unpaired := c.unpaired + int64(len(c.pending))
	for _, body := range c.bodies {
		if !body.used {
			unpaired++
		}
	}
	value := domain.RCAClaudeInline{ProjectID: domain.ProjectID(c.key.ProjectID), SessionID: c.session, CaptureID: c.id, Role: string(c.owner.kind), Requested: true, Active: false, Reason: "", Coverage: "observed_inline_attempt", Method: "utf8_bytes_div4_v1", ObservedAt: time.Now().UTC(), Attempts: delta, Unpaired: unpaired, DroppedBatches: c.dropped}
	s.mu.Unlock()
	// Measure only a numeric value. Raw record/body/attributes are already out of
	// scope; neither this DTO nor the worker closure retains the HTTP request.
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) > claudeBatchBytes {
		s.RejectClaude(c.marker, "projection_limit")
		return "projection_limit"
	}
	a.mu.Lock()
	if a.released {
		a.mu.Unlock()
		return "canceled"
	}
	a.transferred = true
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		a.transferred = false
		a.mu.Unlock()
		return "canceled"
	}
	s.wg.Add(1)
	s.mu.Unlock()
	a.mu.Unlock()
	go func() {
		defer s.wg.Done()
		defer s.core.Finish(a.ticket)
		defer func() {
			if recover() != nil {
				s.core.Unavailable()
				s.RejectClaude(c.marker, "analysis_failed")
			}
		}()
		why := s.core.Commit(a.ticket, func(ctx context.Context) error { return s.projection.StageClaudeInline(ctx, value) }, nil, s.projection.FinishSnapshot)
		if why != "" {
			if why == "storage_unavailable" {
				s.core.Unavailable()
				why = s.storageReason()
			}
			s.RejectClaude(c.marker, why)
		}
	}()
	return ""
}
