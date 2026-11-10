package rca

import (
	"context"
	"net"
	"net/url"
	"strings"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	rcapolicy "github.com/aoagents/agent-orchestrator/components/hcg/rca"
)

type claudeCapability struct {
	Runtime ports.RCAClaudeRuntime
	Role    domain.SessionKind
}

type claudePrepared struct {
	marker, id, generation string
	receipt                rcapolicy.Revision
}

// SetClaudeLaunchEndpoint installs only the existing primary loopback route.
// The production capability map is intentionally empty until separate native
// N0 evidence approves a named binary/bridge/SDK/OS/role. No HTTP grant exists.
func (s *Service) SetClaudeLaunchEndpoint(endpoint string) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Port() == "" || u.Path != "/api/v1/rca/otlp/v1/logs" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return
	}
	if _, _, err := net.SplitHostPort(u.Host); err != nil {
		return
	}
	s.mu.Lock()
	s.claudeEndpoint = endpoint
	s.mu.Unlock()
}

// InvalidateClaudeLaunch withdraws main-launch namespaces before owner replacement.
func (s *Service) InvalidateClaudeLaunch(id domain.SessionID) {
	s.core.Invalidate(rcapolicy.Key{ProjectID: "agent-orchestrator", SessionID: string(id)})
	s.core.Invalidate(rcapolicy.Key{ProjectID: "agent-orchestrator", SessionID: string(id), Role: "orchestrator"})
	s.revokeClaude(id, "source_stopped")
	s.mu.Lock()
	delete(s.pendingClaude, id)
	s.mu.Unlock()
}

// PrepareClaudeEnvironment returns only an approved RCA env delta, or none.
func (s *Service) PrepareClaudeEnvironment(ctx context.Context, n ports.RCAClaudeLaunch, runtime ports.RCAClaudeRuntime, keys []string) map[string]string {
	reason := "unsupported_capability"
	defer func() {
		if reason != "" {
			s.mu.Lock()
			s.claudeReason[n.SessionID] = reason
			s.mu.Unlock()
		}
	}()
	if n.ProjectID != "agent-orchestrator" || (n.Kind != domain.KindWorker && n.Kind != domain.KindOrchestrator) || n.Generation == "" || n.DataRoot == "" || n.DataRoot != s.dataRoot {
		return nil
	}
	// Only non-secret key presence reaches this gate. Do not replace a user's
	// exporter, headers/helper or telemetry setting, even if its value is blank.
	for _, key := range keys {
		upper := strings.ToUpper(key)
		if strings.HasPrefix(upper, "OTEL_") || upper == "CLAUDE_CODE_ENABLE_TELEMETRY" || strings.Contains(upper, "OTEL") && strings.Contains(upper, "HELPER") {
			reason = "exporter_conflict"
			return nil
		}
	}
	s.mu.Lock()
	approved := s.claudeCapabilities[claudeCapability{Runtime: runtime, Role: n.Kind}]
	endpoint := s.claudeEndpoint
	check := s.claudeCapabilityCheck
	s.mu.Unlock()
	if check != nil {
		approved = check(runtime, n.Kind)
	}
	if !approved || runtime.BinaryVersion == "" || runtime.BridgeVersion == "" || runtime.SDKVersion == "" || runtime.OS == "" || endpoint == "" {
		return nil
	}
	rec, inputs, err := s.facts(ctx, n.SessionID)
	if err != nil || !claudeScope(rec) || rec.Kind != n.Kind {
		reason = "scope_not_allowed"
		return nil
	}
	key := keyFor(rec)
	view, why := s.core.CachedView(ctx, key, inputs)
	if why != "" {
		reason = why
		return nil
	}
	if !view.EffectiveEnabled {
		reason = "disabled"
		return nil
	}
	prepared := claudePrepared{marker: uuid.NewString(), id: uuid.NewString(), generation: n.Generation, receipt: s.core.Revision(key)}
	s.mu.Lock()
	if s.closed || s.failed {
		s.mu.Unlock()
		reason = "source_stopped"
		return nil
	}
	s.pendingClaude[n.SessionID] = prepared
	s.mu.Unlock()
	reason = ""
	return map[string]string{
		"CLAUDE_CODE_ENABLE_TELEMETRY": "1", "OTEL_LOGS_EXPORTER": "otlp", "OTEL_METRICS_EXPORTER": "none", "OTEL_TRACES_EXPORTER": "none",
		"OTEL_EXPORTER_OTLP_PROTOCOL": "http/protobuf", "OTEL_EXPORTER_OTLP_LOGS_PROTOCOL": "http/protobuf", "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT": endpoint,
		"OTEL_EXPORTER_OTLP_LOGS_HEADERS": "X-AO-RCA-Capture=" + prepared.marker, "OTEL_LOG_RAW_API_BODIES": "1",
		"OTEL_BLRP_MAX_QUEUE_SIZE": "128", "OTEL_BLRP_MAX_EXPORT_BATCH_SIZE": "16", "OTEL_BLRP_EXPORT_TIMEOUT": "2000",
	}
}

// ClaudeLaunchReady binds a prepared namespace after the AO owner commits.
func (s *Service) ClaudeLaunchReady(ctx context.Context, n ports.RCAClaudeLaunch, live bool) {
	s.mu.Lock()
	prepared, ok := s.pendingClaude[n.SessionID]
	delete(s.pendingClaude, n.SessionID)
	s.mu.Unlock()
	if live || !ok || prepared.generation != n.Generation {
		return
	}
	if !s.core.Current(prepared.receipt) {
		return
	}
	rec, _, err := s.facts(ctx, n.SessionID)
	if err != nil || rec.Metadata.ControllerGeneration != n.Generation || rec.ProjectID != n.ProjectID || rec.Kind != n.Kind {
		return
	}
	_, _ = s.armClaudeCandidate(ctx, rec, prepared)
}

// ClaudeControllerStopped withdraws only the matching controller generation.
func (s *Service) ClaudeControllerStopped(id domain.SessionID, generation string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if pending, ok := s.pendingClaude[id]; ok && pending.generation == generation {
		delete(s.pendingClaude, id)
	}
	c := s.captureSessions[id]
	if c == nil || c.owner.controller != generation {
		return
	}
	s.core.Invalidate(c.key)
	s.dropClaude(c, "source_stopped")
}
