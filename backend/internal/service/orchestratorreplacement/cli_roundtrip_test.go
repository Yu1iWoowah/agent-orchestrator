package orchestratorreplacement_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
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

func TestReplacementResumeRetryCLIReconcilesPositiveEvidence(t *testing.T) {
	for _, outcome := range []string{"not launched", "resumed", "unknown"} {
		t.Run(outcome, func(t *testing.T) {
			x := stoppedSuccessorCoordinatorFixture(t)
			ctx := context.Background()
			authorizations := 0
			x.f.runtime.nativeResume = func(ctx context.Context, op domain.OrchestratorReplacement, allowNew bool) (ports.ReplacementResumeObservation, error) {
				p := op.ResumeEvidence
				if allowNew {
					authorizations++
					if outcome == "not launched" {
						panic("daemon crashed after resume intent before native launch")
					}
					if outcome == "resumed" {
						rec, _, err := x.f.store.GetSession(ctx, op.SuccessorSessionID)
						if err != nil {
							t.Fatal(err)
						}
						rec.Metadata.ControllerGeneration, rec.Metadata.BrowserCapabilityVerifier = p.ResumeGeneration, "after-verifier"
						if err := x.f.store.UpdateSession(ctx, rec); err != nil {
							t.Fatal(err)
						}
						rec, _, err = x.f.store.GetSession(ctx, rec.ID)
						if err != nil {
							t.Fatal(err)
						}
						if ok, err := x.f.store.SetSessionHibernated(ctx, rec.ID, rec.Revision, nil); err != nil || !ok {
							t.Fatal(ok, err)
						}
					}
					return ports.ReplacementResumeObservation{}, ports.ErrChatRecoveryInconclusive
				}
				if outcome == "not launched" {
					return ports.ReplacementResumeObservation{}, ports.ErrChatResumeNotLaunched
				}
				if outcome == "unknown" {
					return ports.ReplacementResumeObservation{}, ports.ErrChatRecoveryInconclusive
				}
				rec, _, err := x.f.store.GetSession(ctx, op.SuccessorSessionID)
				if err != nil {
					t.Fatal(err)
				}
				return ports.ReplacementResumeObservation{Owner: rec.ControllerOwner(), NativeID: p.BeforeNativeID, CapabilityVerifier: rec.Metadata.BrowserCapabilityVerifier, HistoryVerified: true, ObservedAt: time.Now().UTC()}, nil
			}
			op, err := x.service().Advance(ctx, x.operation.ID)
			if (err != nil && outcome != "not launched") || op.Phase != domain.ReplacementRecoveryRequired || outcome == "not launched" && (err == nil || op.Error == nil || op.Error.Code != "ORCHESTRATOR_REPLACEMENT_PANIC") {
				t.Fatal(op.Phase, err)
			}
			intent := op.ResumeEvidence.ResumeGeneration
			router := chi.NewRouter()
			router.Route("/api/v1", func(r chi.Router) { (&controllers.OrchestratorReplacementController{Svc: x.service()}).Register(r) })
			server := httptest.NewServer(router)
			defer server.Close()
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
			var stdout, stderr bytes.Buffer
			command := cli.NewRootCommand(cli.Deps{Out: &stdout, Err: &stderr, ProcessAlive: func(int) bool { return true }})
			command.SetArgs([]string{"orchestrator", "replacement", "retry", op.ID, "--json"})
			if err := command.ExecuteContext(ctx); err != nil {
				t.Fatal(err, stderr.String())
			}
			if !strings.Contains(stdout.String(), "retainedRetry") {
				t.Fatal("original retry cause absent", stdout.String())
			}
			op, err = x.service().Advance(ctx, op.ID)
			if err != nil {
				t.Fatal(err)
			}
			switch outcome {
			case "not launched":
				if p := op.ResumeEvidence; p.ResumeIntentAt != nil || len(p.UnlaunchedAttempts) != 1 || p.UnlaunchedAttempts[0].Generation != intent || p.ResumedAt != nil {
					t.Fatal("no-launch evidence not retained", op.Phase, p)
				}
			case "resumed":
				if p := op.ResumeEvidence; p.ResumedAt == nil || !p.HistoryVerified || p.AfterOwner.ControllerGeneration != intent || p.AfterNativeID != p.BeforeNativeID {
					t.Fatal("same native positive receipt missing", op.Phase, p)
				}
			case "unknown":
				if p := op.ResumeEvidence; p.ResumedAt != nil || p.ResumeGeneration != intent || len(p.UnlaunchedAttempts) != 0 {
					t.Fatal("unknown outcome gained facts", op.Phase, p)
				}
			}
			if authorizations != 1 {
				t.Fatal("retry launched another provider", authorizations)
			}
			if outcome == "not launched" {
				originalID, originalSuccessor := op.ID, op.SuccessorSessionID
				if _, err := x.service().Retry(ctx, op.ID); err != nil {
					t.Fatal(err)
				}
				x.f.runtime.nativeResume = func(ctx context.Context, op domain.OrchestratorReplacement, allowNew bool) (ports.ReplacementResumeObservation, error) {
					if !allowNew {
						t.Fatal("next attempt did not receive explicit launch authority")
					}
					authorizations++
					rec, _, err := x.f.store.GetSession(ctx, op.SuccessorSessionID)
					if err != nil {
						t.Fatal(err)
					}
					rec.Metadata.ControllerGeneration, rec.Metadata.BrowserCapabilityVerifier = op.ResumeEvidence.ResumeGeneration, "verified-next-launch"
					if err := x.f.store.UpdateSession(ctx, rec); err != nil {
						t.Fatal(err)
					}
					rec, _, err = x.f.store.GetSession(ctx, rec.ID)
					if err != nil {
						t.Fatal(err)
					}
					if ok, err := x.f.store.SetSessionHibernated(ctx, rec.ID, rec.Revision, nil); err != nil || !ok {
						t.Fatal(ok, err)
					}
					return ports.ReplacementResumeObservation{Owner: rec.ControllerOwner(), NativeID: rec.Metadata.ProviderConversationID, CapabilityVerifier: rec.Metadata.BrowserCapabilityVerifier, HistoryVerified: true, ObservedAt: time.Now().UTC()}, nil
				}
				op, err = x.service().Advance(ctx, op.ID)
				if err != nil || op.ID != originalID || op.SuccessorSessionID != originalSuccessor || op.ResumeEvidence.ResumedAt == nil || len(op.ResumeEvidence.UnlaunchedAttempts) != 1 || op.ResumeEvidence.ResumeGeneration == intent || authorizations != 2 {
					t.Fatal("positive no-launch did not authorize a recoverable next attempt", op.Phase, err)
				}
			}

			if _, err := x.service().Cancel(ctx, op.ID); err == nil || errors.Is(err, context.Canceled) {
				t.Fatal("post-stop cancel allowed", err)
			}
		})
	}
}
