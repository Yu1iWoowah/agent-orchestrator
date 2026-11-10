package rca

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func claudeFlow(t *testing.T, kind domain.SessionKind) (*Service, *scopeFixture, *DiskStore, string) {
	t.Helper()
	svc, metadata, disk, _ := flowSetup(t)
	metadata.sessions[0].Harness = domain.HarnessClaudeCode
	metadata.sessions[0].Kind = kind
	marker, why := svc.armClaude(t.Context(), metadata.sessions[0])
	if why != "" {
		t.Fatal(why)
	}
	return svc, metadata, disk, marker
}
func sendClaude(t *testing.T, svc *Service, marker string, attributes ...map[string]string) string {
	t.Helper()
	admission, why := svc.AdmitClaude(t.Context(), marker)
	if why != "" {
		t.Fatal(why)
	}
	defer admission.Release()
	records := make([][]byte, len(attributes))
	for i, attr := range attributes {
		attr["session.id"] = syntheticNativeID
		records[i] = otlpRecord(attr)
	}
	batch := otlpBatch(records...)
	return admission.Read(bytes.NewReader(batch), int64(len(batch)))
}
func awaitClaude(t *testing.T, svc *Service) domain.RCAComposition {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		out, err := svc.Get(t.Context(), "selected", domain.RCAQuery{Limit: 200})
		if err != nil {
			t.Fatal(err)
		}
		if out.Reason != "writer_busy" {
			return out
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("numeric writer did not complete")
	return domain.RCAComposition{}
}
func TestClaudeTwoRolesAttemptsAndCanonicalSeparation(t *testing.T) {
	for _, kind := range []domain.SessionKind{domain.KindWorker, domain.KindOrchestrator} {
		t.Run(string(kind), func(t *testing.T) {
			svc, _, disk, marker := claudeFlow(t, kind)
			body := func(request string) map[string]string {
				return map[string]string{"event.name": "api_request_body", "request_body_id": "body-1", "request_id": request, "body": `{"messages":[{"role":"user","content":"A_SYNTHETIC_秘密 B_SYNTHETIC_CONTENT"}]}`}
			}
			if why := sendClaude(t, svc, marker, body("attempt-1"), body("attempt-1"), body("retry-2")); why != "" {
				t.Fatal(why)
			}
			out := awaitClaude(t, svc)
			if out.ClaudeInline == nil || !out.ClaudeInline.Active || len(out.ClaudeInline.Attempts) != 2 || out.Recorded != nil || out.RequestComposition != "UNKNOWN" || len(out.Canonical.Events) != 1 {
				t.Fatalf("incorrect Claude projection: %+v", out.ClaudeInline)
			}
			if out.ClaudeInline.Role != string(kind) || out.ClaudeInline.Attempts[1].Ordinal != 2 || out.ClaudeInline.Attempts[1].CanonicalEventID != nil {
				t.Fatal("attempt identity/role/canonical guessed")
			}
			if why := sendClaude(t, svc, marker, map[string]string{"event.name": "api_response_body", "request_id": "attempt-1", "message.id": "message-1", "body": `{"usage":{"input_tokens":999}}`}); why != "" {
				t.Fatal(why)
			}
			out = awaitClaude(t, svc)
			if len(out.ClaudeInline.Attempts) != 2 || len(out.Canonical.Events) != 1 || *out.Canonical.Events[0].InputTokens != 100 {
				t.Fatal("response usage counted twice")
			}
			encoded, _ := json.Marshal(out)
			raw, err := disk.dir.ReadFile("state.json")
			if err != nil {
				t.Fatal(err)
			}
			for _, canary := range []string{"A_SYNTHETIC", "B_SYNTHETIC", "秘密", "attempt-1", "retry-2", "body-1", "message-1", syntheticNativeID, marker} {
				if bytes.Contains(encoded, []byte(canary)) || bytes.Contains(raw, []byte(canary)) {
					t.Fatal("content/native identity persisted")
				}
			}
		})
	}
}
func TestClaudeMetadataFirstAndConflictingReplay(t *testing.T) {
	svc, _, _, marker := claudeFlow(t, domain.KindWorker)
	if why := sendClaude(t, svc, marker, map[string]string{"event.name": "api_request", "request_id": "r1", "request_body_id": "b1"}); why != "" {
		t.Fatal(why)
	}
	_ = awaitClaude(t, svc)
	if why := sendClaude(t, svc, marker, map[string]string{"event.name": "api_request_body", "request_body_id": "b1", "body": `{"system":"hello"}`}); why != "" {
		t.Fatal(why)
	}
	out := awaitClaude(t, svc)
	if len(out.ClaudeInline.Attempts) != 1 || *out.ClaudeInline.Attempts[0].Buckets[0].Bytes != 5 {
		t.Fatal("metadata/body pairing lost")
	}
	if why := sendClaude(t, svc, marker, map[string]string{"event.name": "api_request_body", "request_body_id": "b1", "body": `{"system":"other"}`}); why != "identity_conflict" {
		t.Fatal(why)
	}
	if _, why := svc.AdmitClaude(t.Context(), marker); why != "source_stopped" {
		t.Fatal("conflict did not revoke marker")
	}
}
func TestClaudeOffOnNeverRestoresMarker(t *testing.T) {
	svc, metadata, _, marker := claudeFlow(t, domain.KindOrchestrator)
	off, on := false, true
	if _, err := svc.SetOverride(t.Context(), "selected", &off); err != nil {
		t.Fatal(err)
	}
	if _, why := svc.AdmitClaude(t.Context(), marker); why != "source_stopped" {
		t.Fatal(why)
	}
	if _, err := svc.SetOverride(t.Context(), "selected", &on); err != nil {
		t.Fatal(err)
	}
	if _, why := svc.AdmitClaude(t.Context(), marker); why != "source_stopped" {
		t.Fatal("ABA marker revived")
	}
	fresh, why := svc.armClaude(t.Context(), metadata.sessions[0])
	if why != "" || fresh == marker {
		t.Fatal("new safe launch did not create new namespace")
	}
	if err := svc.EmergencyStop(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	if err := svc.EmergencyStop(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	if _, why := svc.AdmitClaude(t.Context(), fresh); why != "source_stopped" {
		t.Fatal("host resume revived marker")
	}
}
func TestClaudeSelectedScopeAndGeneration(t *testing.T) {
	for _, change := range []func(*domain.SessionRecord){
		func(r *domain.SessionRecord) { r.ProjectID = "other" },
		func(r *domain.SessionRecord) { r.Kind = domain.SessionKind("reviewer") },
		func(r *domain.SessionRecord) { r.Mode = domain.SessionModeTUI },
		func(r *domain.SessionRecord) { r.Harness = domain.HarnessCodex; r.Kind = domain.KindOrchestrator },
	} {
		svc, metadata, _, _ := claudeFlow(t, domain.KindWorker)
		rec := metadata.sessions[0]
		change(&rec)
		if _, why := svc.armClaude(context.Background(), rec); why != "scope_not_allowed" {
			t.Fatalf("invalid scope admitted: %s", why)
		}
	}
	svc, metadata, _, marker := claudeFlow(t, domain.KindWorker)
	metadata.sessions[0].Metadata.ControllerGeneration = "new-generation"
	out := awaitClaude(t, svc)
	if out.ClaudeInline.Active {
		t.Fatal("changed owner still active")
	}
	if _, why := svc.AdmitClaude(t.Context(), marker); why != "source_stopped" {
		t.Fatal("old generation admitted")
	}
}
func TestClaudeIndexAndProjectionLimitsStopOnlyCapture(t *testing.T) {
	svc, _, _, marker := claudeFlow(t, domain.KindWorker)
	s := svc.captureSessions["selected"]
	svc.mu.Lock()
	s.entries = claudeIndexEntries
	svc.mu.Unlock()
	if why := sendClaude(t, svc, marker, map[string]string{"event.name": "api_request_body", "request_id": "r1", "request_body_id": "b1", "body": `{"system":"test"}`}); why != "capture_limit" {
		t.Fatal(why)
	}
	if out := awaitClaude(t, svc); len(out.Canonical.Events) != 1 {
		t.Fatal("native/canonical path stopped")
	}
	for _, n := range []int{256, 257} {
		raw := otlpBatch(otlpRecord(map[string]string{"event.name": "api_request_body", "session.id": strings.Repeat("x", n)}))
		why := readClaudeOTLP(t.Context(), bytes.NewReader(raw), int64(len(raw)), func(claudeEvent) string { return "" })
		if n == 256 && why != "" || n == 257 && why != "identity_limit" {
			t.Fatal(why)
		}
	}
}

func TestClaudeTransientIndexEntryAndHostBudgets(t *testing.T) {
	for _, boundary := range []string{"entries", "bytes"} {
		t.Run(boundary, func(t *testing.T) {
			svc, _, _, marker := claudeFlow(t, domain.KindWorker)
			admission, why := svc.AdmitClaude(t.Context(), marker)
			if why != "" {
				t.Fatal(why)
			}
			defer admission.Release()
			lease := admission.(*claudeAdmission)
			capture := lease.capture
			svc.mu.Lock()
			if boundary == "entries" {
				capture.entries = claudeIndexEntries - 1
			} else {
				svc.indexBytes = claudeIndexBytes - (1024 + len("b1"))
			}
			svc.mu.Unlock()
			var delta []domain.RCAClaudeAttempt
			event := claudeEvent{name: "api_request_body", session: syntheticNativeID, bodyID: "b1", body: []byte(`{"system":"test"}`)}
			if why := lease.event(event, &delta); why != "" {
				t.Fatal("at-limit admission:", why)
			}
			event.bodyID = "b2"
			if why := lease.event(event, &delta); why != "capture_limit" {
				t.Fatal("above-limit admission:", why)
			}
		})
	}
}
func TestClaudeNumericRowsLimitAndUnprovedChild(t *testing.T) {
	svc, _, _, marker := claudeFlow(t, domain.KindWorker)
	admission, why := svc.AdmitClaude(t.Context(), marker)
	if why != "" {
		t.Fatal(why)
	}
	defer admission.Release()
	lease := admission.(*claudeAdmission)
	var delta []domain.RCAClaudeAttempt
	child := claudeEvent{name: "api_request_body", session: "unproved-child", bodyID: "child", requestID: "child", body: []byte(`{"system":"must-not-count"}`)}
	child.session = syntheticNativeID
	child.unprovedChild = true
	if why := lease.event(child, &delta); why != "" || len(delta) != 0 || len(lease.capture.bodies) != 0 {
		t.Fatal("child was attributed to the parent")
	}
	for n := 0; n < 86; n++ {
		key := fmt.Sprintf("r-%d", n)
		event := claudeEvent{name: "api_request_body", session: syntheticNativeID, bodyID: key, requestID: key, body: []byte(`{"system":"test"}`)}
		why := lease.event(event, &delta)
		if n < 85 && why != "" || n == 85 && why != "projection_limit" {
			t.Fatalf("attempt=%d reason=%s", n, why)
		}
	}
	if len(delta)*6 != 510 {
		t.Fatal("row budget not bounded")
	}
}
