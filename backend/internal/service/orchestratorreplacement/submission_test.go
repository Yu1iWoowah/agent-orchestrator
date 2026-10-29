package orchestratorreplacement_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	replacement "github.com/aoagents/agent-orchestrator/backend/internal/service/orchestratorreplacement"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/projectdocuments"
)

type sourceCapability struct{}

func (sourceCapability) Valid(_ domain.SessionID, token, verifier string) bool {
	return token == "author-secret" && verifier == "verifier"
}

func operationHandoff(t *testing.T, op domain.OrchestratorReplacement) string {
	t.Helper()
	b, _ := preparedDocument(t)
	meta := replacement.PreparationMetadata{Version: 1, OperationID: op.ID, AttemptID: op.Preparation.AttemptID, Nonce: op.Preparation.Nonce, SourceSessionID: op.SourceSessionID, LeaseEpoch: op.Preparation.LeaseEpoch}
	mb, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	return "<!-- ao-handoff " + string(mb) + " -->" + string(b[strings.IndexByte(string(b), '\n'):])
}

func TestReplacementPreparedValidatesActualManagedDocument(t *testing.T) {
	for _, which := range []string{"valid", "missing section", "wrong nonce"} {
		t.Run(which, func(t *testing.T) {
			f := setup(t)
			ctx := context.Background()
			documents := projectdocuments.New(projectdocuments.Deps{Store: f.store, DataDir: t.TempDir(), Capabilities: sourceCapability{}})
			svc := replacement.New(replacement.Deps{Store: f.store, Runtime: f.runtime, Documents: documents})
			op, err := svc.Accept(ctx, f.request)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			for _, phase := range []domain.ReplacementPhase{domain.ReplacementPreflight, domain.ReplacementWaitingSourceIdle} {
				op.Phase = phase
				if saved, err := f.store.SaveOrchestratorReplacement(ctx, op); err != nil || !saved {
					t.Fatal(saved, err)
				}
				op.Revision++
			}
			op.Phase = domain.ReplacementAdoptingSharedFiles
			op.Preparation = &domain.ReplacementPreparation{AttemptID: "22222222-2222-4222-8222-222222222222", Nonce: strings.Repeat("c", 64), ControlClientID: "prepare/" + op.ID, StartedAt: now, DeadlineAt: now.Add(time.Minute), ExpectedSessions: []domain.SessionID{"demo-2", "demo-3"}}
			if ok, err := f.store.SaveOrchestratorReplacement(ctx, op); err != nil || !ok {
				t.Fatal(ok, err)
			}
			op, _, err = f.store.GetOrchestratorReplacement(ctx, op.ID)
			if err != nil {
				t.Fatal(err)
			}
			op, err = documents.BeginReplacementPreparation(ctx, op.ID, op.Revision)
			if err != nil {
				t.Fatal(err)
			}
			actor := projectdocuments.Actor{SessionID: op.SourceSessionID, Capability: "author-secret"}
			grant, err := documents.Acquire(ctx, op.ProjectID, actor)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := preparedDocument(t)
			meta := replacement.PreparationMetadata{Version: 1, OperationID: op.ID, AttemptID: op.Preparation.AttemptID, Nonce: op.Preparation.Nonce, SourceSessionID: op.SourceSessionID, LeaseEpoch: op.Preparation.LeaseEpoch}
			mb, err := json.Marshal(meta)
			if err != nil {
				t.Fatal(err)
			}
			content := "<!-- ao-handoff " + string(mb) + " -->" + string(b[strings.IndexByte(string(b), '\n'):])
			if which == "missing section" {
				content = strings.Split(content, "## F")[0]
			}
			file, err := documents.Write(ctx, op.ProjectID, projectdocuments.WriteInput{Actor: actor, Lease: grant, Path: op.DocumentPath, Content: content, ExpectedHash: projectdocuments.MissingHash})
			if err != nil {
				t.Fatal(err)
			}
			in := projectdocuments.ReplacementPreparationInput{Actor: actor, AttemptID: meta.AttemptID, Nonce: meta.Nonce, Path: op.DocumentPath, Hash: file.Hash, WriteReceiptID: file.WriteReceiptID}
			if which == "wrong nonce" {
				in.Nonce = "stale"
			}
			submitted, err := svc.Prepared(ctx, op.ID, in)
			if which == "missing section" {
				code(t, err, "ORCHESTRATOR_HANDOFF_CONTENT_INVALID")
				return
			}
			if which == "wrong nonce" {
				code(t, err, "ORCHESTRATOR_HANDOFF_RECEIPT_INVALID")
				return
			}
			if err != nil || submitted.Preparation.SubmittedHash != file.Hash || submitted.Preparation.VerifiedAt != nil || submitted.Phase != domain.ReplacementPreparingHandoff {
				t.Fatalf("prepared receipt changed completion semantics: %+v %v", submitted, err)
			}
			serialized, err := json.Marshal(submitted)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(serialized), "author-secret") {
				t.Fatal("operation persisted source capability")
			}
			state, err := f.store.GetProjectDocumentState(ctx, op.ProjectID)
			if err != nil || state.Lease.Frozen {
				t.Fatal("submitted receipt froze an unsettled turn", err)
			}
		})
	}
}
