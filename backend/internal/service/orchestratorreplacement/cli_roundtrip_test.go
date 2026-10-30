package orchestratorreplacement_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/cli"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	"github.com/aoagents/agent-orchestrator/backend/internal/runfile"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/projectdocuments"
)

func TestPreparationPromptCLIIntakeAndPreparedRoundtrip(t *testing.T) {
	x := preparingExportFixture(t)
	ctx, op := context.Background(), x.operation
	actor := projectdocuments.Actor{SessionID: op.SourceSessionID, Capability: "author-secret"}
	grant, err := x.documents.Acquire(ctx, op.ProjectID, actor)
	if err != nil {
		t.Fatal(err)
	}
	file, err := x.documents.Write(ctx, op.ProjectID, projectdocuments.WriteInput{Actor: actor, Lease: grant, Path: op.DocumentPath, Content: operationHandoff(t, op), ExpectedHash: projectdocuments.MissingHash})
	if err != nil {
		t.Fatal(err)
	}
	router := chi.NewRouter()
	router.Route("/api/v1", func(r chi.Router) { (&controllers.OrchestratorReplacementController{Svc: x.service()}).Register(r) })
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	runPath := filepath.Join(t.TempDir(), "running.json")
	if err := runfile.Write(runPath, runfile.Info{PID: os.Getpid(), Port: port, StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AO_RUN_FILE", runPath)
	t.Setenv("AO_DATA_DIR", t.TempDir())
	t.Setenv("AO_SESSION_ID", string(op.SourceSessionID))
	t.Setenv("AO_BROWSER_CAPABILITY", "author-secret")
	run := func(args ...string) string {
		t.Helper()
		var stdout, stderr bytes.Buffer
		command := cli.NewRootCommand(cli.Deps{Out: &stdout, Err: &stderr, ProcessAlive: func(int) bool { return true }})
		command.SetArgs(append([]string{"orchestrator", "replacement"}, args...))
		if err := command.ExecuteContext(ctx); err != nil {
			t.Fatalf("CLI roundtrip: %v; stderr=%s", err, stderr.String())
		}
		if strings.Contains(stdout.String()+stderr.String(), "author-secret") {
			t.Fatal("source capability leaked")
		}
		return stdout.String()
	}
	var intake struct {
		Intake domain.ReplacementIntake `json:"intake"`
	}
	if err := json.Unmarshal([]byte(run("intake", op.ID, "--json")), &intake); err != nil || intake.Intake.OperationID != op.ID {
		t.Fatal("intake receipt missing", err)
	}
	out := run("prepared", op.ID, "--attempt", op.Preparation.AttemptID, "--nonce", op.Preparation.Nonce, "--path", op.DocumentPath, "--hash", file.Hash, "--write-receipt-id", file.WriteReceiptID, "--json")
	var receipt controllers.OrchestratorReplacementResponse
	if err := json.Unmarshal([]byte(out), &receipt); err != nil {
		t.Fatal(err)
	}
	if p := receipt.Operation.Preparation; p == nil || p.SubmittedHash != file.Hash || p.WriteReceiptID != file.WriteReceiptID || p.VerifiedAt != nil {
		t.Fatal("submission changed verification boundary", out)
	}
	current, err := x.service().Get(ctx, op.ID)
	if err != nil || current.Phase != domain.ReplacementPreparingHandoff || current.FrozenAt != nil || current.SourceStopIntentAt != nil || *x.actualStops != 0 {
		t.Fatal("CLI receipt performed freeze or stop", err)
	}
}
