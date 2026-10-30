package controllers_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
)

type replacementOperatorAPI struct {
	replacementAPI
	repairCalls   int
	input         domain.ReplacementRepairRequest
	snapshotReads int
}

func (s *replacementOperatorAPI) Diagnose(context.Context, string) (domain.ReplacementDiagnostic, error) {
	return domain.ReplacementDiagnostic{OperationID: replacementAPIID, ProjectID: "demo", SourceSessionID: "source", Revision: 8, Active: true, RawDigest: strings.Repeat("a", 64), Corrupt: true, CorruptionReason: "private-raw-field", SnapshotRevision: 7, SnapshotDigest: strings.Repeat("b", 64)}, nil
}
func (s *replacementOperatorAPI) RepairSnapshot(context.Context, string) (domain.ReplacementRepairSnapshot, error) {
	s.snapshotReads++
	if len(s.op.Repairs) > 0 {
		return domain.ReplacementRepairSnapshot{OperationID: replacementAPIID, Revision: 9, Digest: strings.Repeat("c", 64), Payload: []byte("private-new-snapshot")}, nil
	}
	return domain.ReplacementRepairSnapshot{OperationID: replacementAPIID, Revision: 7, Digest: strings.Repeat("b", 64), Payload: []byte("private-sealed-bytes")}, nil
}

func TestReplacementRepairHTTPReplaysPositiveReceiptAfterLostResponse(t *testing.T) {
	op := replacementAPIState()
	op.Repairs = []domain.ReplacementRepairReceipt{{BeforeRevision: 8, BeforeDigest: strings.Repeat("a", 64), SnapshotRevision: 7, SnapshotDigest: strings.Repeat("b", 64), AfterRevision: 9, RepairedAt: op.UpdatedAt}}
	op.Phase = domain.ReplacementRecoveryRequired
	svc := &replacementOperatorAPI{replacementAPI: replacementAPI{op: op}}
	router := chi.NewRouter()
	(&controllers.OrchestratorReplacementController{Svc: svc}).Register(router)
	body := `{"expectedRevision":8,"expectedDigest":"` + strings.Repeat("a", 64) + `","snapshotRevision":7,"snapshotDigest":"` + strings.Repeat("b", 64) + `"}`
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/orchestrator/replacements/"+replacementAPIID+"/repair", strings.NewReader(body)))
	if w.Code != http.StatusAccepted || svc.repairCalls != 0 || !strings.Contains(w.Body.String(), "recovery_required") || strings.Contains(w.Body.String(), "private-new-snapshot") {
		t.Fatalf("lost repair response was not a read of its positive receipt: %d calls=%d", w.Code, svc.repairCalls)
	}
}

func TestReplacementRepairHTTPRequiresBothRevisionFields(t *testing.T) {
	for _, missing := range []string{"expectedRevision", "snapshotRevision"} {
		t.Run(missing, func(t *testing.T) {
			svc := &replacementOperatorAPI{replacementAPI: replacementAPI{op: replacementAPIState()}}
			router := chi.NewRouter()
			(&controllers.OrchestratorReplacementController{Svc: svc}).Register(router)
			body := map[string]any{"expectedRevision": 0, "snapshotRevision": 7, "expectedDigest": strings.Repeat("a", 64), "snapshotDigest": strings.Repeat("b", 64)}
			delete(body, missing)
			raw, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/orchestrator/replacements/"+replacementAPIID+"/repair", strings.NewReader(string(raw))))
			if w.Code != http.StatusBadRequest || svc.snapshotReads != 0 || svc.repairCalls != 0 {
				t.Fatalf("missing %s reached repair boundary: status=%d reads=%d writes=%d", missing, w.Code, svc.snapshotReads, svc.repairCalls)
			}
		})
	}
}
func (s *replacementOperatorAPI) Repair(_ context.Context, in domain.ReplacementRepairRequest) (domain.OrchestratorReplacement, error) {
	s.repairCalls++
	s.input = in
	return s.op, nil
}

func TestReplacementOperatorHTTPRequiresExplicitSnapshotCASAndHidesRawState(t *testing.T) {
	svc := &replacementOperatorAPI{replacementAPI: replacementAPI{op: replacementAPIState()}}
	router := chi.NewRouter()
	(&controllers.OrchestratorReplacementController{Svc: svc}).Register(router)
	diag := httptest.NewRecorder()
	router.ServeHTTP(diag, httptest.NewRequest(http.MethodGet, "/orchestrator/replacements/"+replacementAPIID+"/diagnose", nil))
	if diag.Code != http.StatusOK || !strings.Contains(diag.Body.String(), `"snapshotRevision":7`) || strings.Contains(diag.Body.String(), "private-") || strings.Contains(diag.Body.String(), "corruptionReason") {
		t.Fatalf("unsafe diagnostic %d %s", diag.Code, diag.Body.String())
	}
	for _, which := range []string{"stale snapshot", "matching snapshot"} {
		t.Run(which, func(t *testing.T) {
			digest := strings.Repeat("b", 64)
			if which == "stale snapshot" {
				digest = strings.Repeat("c", 64)
			}
			w := httptest.NewRecorder()
			body := `{"expectedRevision":8,"expectedDigest":"` + strings.Repeat("a", 64) + `","snapshotRevision":7,"snapshotDigest":"` + digest + `"}`
			router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/orchestrator/replacements/"+replacementAPIID+"/repair", strings.NewReader(body)))
			if which == "stale snapshot" {
				if w.Code != http.StatusConflict || svc.repairCalls != 0 {
					t.Fatalf("stale authorization reached repair: %d", w.Code)
				}
				return
			}
			if w.Code != http.StatusAccepted || svc.repairCalls != 1 || svc.input.ExpectedRevision != 8 || string(svc.input.Snapshot.Payload) != "private-sealed-bytes" || strings.Contains(w.Body.String(), "private-sealed-bytes") {
				t.Fatalf("repair lost explicit authority: %d %s", w.Code, w.Body.String())
			}
		})
	}
}
