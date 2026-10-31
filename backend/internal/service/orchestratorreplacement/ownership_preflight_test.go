package orchestratorreplacement_test

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
)

func TestReplacementHistoricalOwnershipFailsBeforeSourceStop(t *testing.T) {
	for _, boundary := range []string{"preflight", "before stop intent"} {
		t.Run(boundary, func(t *testing.T) {
			x := preparedExportFixture(t)
			ctx := context.Background()
			now := time.Now().UTC()

			// Seed retained, settled history without dispatching new fenced work.
			db, err := sql.Open("sqlite", filepath.Join(x.f.store.dataDir, "ao.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			_, err = db.Exec(`INSERT INTO conversation_turns(id,conversation_id,handled_by_session_id,provider_turn_id,controller_generation,state,requested_at,started_at,completed_at,branch_id) VALUES ('old-turn',?,?, 'old-provider-turn',?,'completed',?,?,?,(SELECT active_branch_id FROM conversations WHERE id=?))`, x.conversation.ID, x.source.ID, x.source.Metadata.ControllerGeneration, now.Add(-time.Hour), now.Add(-time.Hour), now.Add(-time.Hour), x.conversation.ID)
			if err != nil {
				t.Fatal(err)
			}

			op := x.operation
			if boundary == "preflight" {
				if _, err := x.service().Cancel(ctx, op.ID); err != nil {
					t.Fatal(err)
				}
				request := x.f.request
				request.OperationKey = "22222222-2222-4222-8222-222222222222"
				var err error
				op, err = x.service().Accept(ctx, request)
				if err != nil {
					t.Fatal(err)
				}
			}
			for range 20 {
				if op.Error != nil {
					break
				}
				var err error
				op, err = x.service().Advance(ctx, op.ID)
				if err != nil {
					t.Fatal(err)
				}
			}
			if op.Error == nil || op.Error.Code != "TRANSCRIPT_PROVENANCE_UNKNOWN" || op.Error.Retryable || op.SourceStopIntentAt != nil || op.SourceStop != nil || x.f.runtime.quiescences != 0 || x.reader.captures != 0 {
				t.Errorf("ownership gate crossed: phase=%s failure=%+v stopIntent=%t stopped=%t quiescences=%d captures=%d", op.Phase, op.Error, op.SourceStopIntentAt != nil, op.SourceStop != nil, x.f.runtime.quiescences, x.reader.captures)
			}
			source, _, err := x.f.store.GetSession(ctx, x.source.ID)
			if err != nil || source.IsTerminated || source.HibernatedAt != nil {
				t.Errorf("source no longer live: hibernated=%t err=%v", source.HibernatedAt != nil, err)
			}
			router := chi.NewRouter()
			(&controllers.OrchestratorReplacementController{Svc: x.service()}).Register(router)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/orchestrator/replacements/"+op.ID+"/cancel", nil))
			if response.Code != http.StatusAccepted {
				t.Errorf("cancel status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}
