package orchestratorreplacement_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	replacement "github.com/aoagents/agent-orchestrator/backend/internal/service/orchestratorreplacement"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/projectdocuments"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/transcriptexport"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

type testStore struct {
	*sqlite.Store
	snapshot      domain.TranscriptSourceSnapshot
	snapshotReads int

	dataDir string
}

func (s *testStore) LoadTranscriptSourceSnapshot(context.Context, domain.SessionID) (domain.TranscriptSourceSnapshot, error) {
	s.snapshotReads++
	return s.snapshot, nil
}

type runtime struct {
	retirements, successors    int
	quiescences                int
	quiesce                    func(context.Context, domain.OrchestratorReplacement) (domain.ReplacementControllerStop, error)
	prepare                    func(context.Context, domain.OrchestratorReplacement, string, bool) (replacement.PreparationDelivery, error)
	preflights                 int
	err                        error
	preflight                  func()
	target                     *domain.ReplacementTarget
	observation                replacement.SourceObservation
	observationErr             error
	preparations, observations int
	prompts                    []string
}

func (r *runtime) ObserveReplacementSource(context.Context, domain.OrchestratorReplacement) (replacement.SourceObservation, error) {
	r.observations++
	return r.observation, r.observationErr
}
func (r *runtime) EnsureReplacementPreparation(ctx context.Context, op domain.OrchestratorReplacement, prompt string, allowNew bool) (replacement.PreparationDelivery, error) {
	r.preparations++
	r.prompts = append(r.prompts, prompt)
	if r.prepare != nil {
		return r.prepare(ctx, op, prompt, allowNew)
	}
	return replacement.PreparationDelivery{TurnID: "owned-control-turn", Owner: op.SourceOwner, NativeID: r.observation.NativeID}, nil
}

func (r *runtime) QuiesceReplacementSource(ctx context.Context, op domain.OrchestratorReplacement) (domain.ReplacementControllerStop, error) {
	r.quiescences++
	if r.quiesce != nil {
		return r.quiesce(ctx, op)
	}
	return domain.ReplacementControllerStop{}, errors.New("unexpected source quiescence")
}

func (r *runtime) PreflightReplacementTarget(context.Context, domain.ProjectRecord) (domain.ReplacementTarget, error) {
	r.preflights++
	if r.preflight != nil {
		r.preflight()
	}
	if r.target != nil {
		return *r.target, r.err
	}
	return domain.ReplacementTarget{AgentConfig: domain.AgentConfig{Model: "sonnet"}, AccountID: "preflight-account"}, r.err
}

func TestReplacementRejectsConfigChangeDuringPreflight(t *testing.T) {
	f := setup(t)
	f.runtime.preflight = func() {
		f.project.Config.Orchestrator.Harness = domain.HarnessCodex
		if err := f.store.UpsertProject(context.Background(), f.project); err != nil {
			t.Fatal(err)
		}
	}
	_, err := f.svc.Accept(context.Background(), f.request)
	code(t, err, "ORCHESTRATOR_REPLACEMENT_CONFIG_CHANGED")
	ops, err := f.store.ListActiveOrchestratorReplacements(context.Background())
	if err != nil || len(ops) != 0 {
		t.Fatal("stale preflight obtained project admission")
	}
}

func TestReplacementPersistsPreflightAccountChoice(t *testing.T) {
	f := setup(t)
	op, err := f.svc.Accept(context.Background(), f.request)
	if err != nil {
		t.Fatal(err)
	}
	if op.TargetAccountID != "preflight-account" {
		t.Fatalf("operation lost preflight account: %q", op.TargetAccountID)
	}
	got, err := f.svc.Get(context.Background(), op.ID)
	if err != nil || got.TargetAccountID != op.TargetAccountID {
		t.Fatalf("reread changed selected account: %q, %v", got.TargetAccountID, err)
	}
}

type documents struct {
	root  string
	calls int
}

func (d *documents) RecordReplacementPreparation(context.Context, string, int64, projectdocuments.ReplacementPreparationInput, func(projectdocuments.File) error) (domain.OrchestratorReplacement, error) {
	return domain.OrchestratorReplacement{}, errors.New("unexpected preparation submission")
}
func (d *documents) BeginReplacementPreparation(context.Context, string, int64) (domain.OrchestratorReplacement, error) {
	return domain.OrchestratorReplacement{}, errors.New("unexpected document preparation")
}
func (d *documents) FreezeReplacementHandoff(context.Context, string, int64) (domain.OrchestratorReplacement, error) {
	return domain.OrchestratorReplacement{}, errors.New("unexpected document freeze")
}

func (d *documents) PublishReplacementEvidence(context.Context, string, int64, transcriptexport.Output) (domain.OrchestratorReplacement, error) {
	return domain.OrchestratorReplacement{}, errors.New("unexpected evidence publication")
}

func (d *documents) Info(context.Context, domain.ProjectID) (projectdocuments.Info, error) {
	d.calls++
	return projectdocuments.Info{SharedDir: d.root, DocumentPath: "handoff/orchestrator-handoff.md"}, nil
}

type fixture struct {
	svc     *replacement.Service
	store   *testStore
	runtime *runtime
	docs    *documents
	source  domain.SessionRecord
	request replacement.Request
	project domain.ProjectRecord
}

func setup(t *testing.T) fixture {
	t.Helper()
	ctx := context.Background()
	dataDir := t.TempDir()
	st := &testStore{Store: sqlitetest.MustOpenAt(t, dataDir), dataDir: dataDir}
	p := domain.ProjectRecord{ID: "demo", Path: t.TempDir(), RegisteredAt: time.Now(), Config: domain.ProjectConfig{Orchestrator: domain.RoleOverride{Harness: domain.HarnessClaudeCode}}}
	if err := st.UpsertProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	source, err := st.CreateSession(ctx, domain.SessionRecord{ProjectID: "demo", Kind: domain.KindOrchestrator, Harness: domain.HarnessClaudeCode, Mode: domain.SessionModeChat, CreatedAt: now, UpdatedAt: now, Metadata: domain.SessionMetadata{ControllerGeneration: "source-generation", ProviderConversationID: "native-source", BrowserCapabilityVerifier: "verifier"}})
	if err != nil {
		t.Fatal(err)
	}
	st.snapshot = domain.TranscriptSourceSnapshot{Session: source, Fingerprint: "ao-snapshot", NativeSessions: []domain.TranscriptNativeSession{{Harness: domain.HarnessClaudeCode, NativeID: "native-source", ConfigDir: t.TempDir()}}}
	r, d := &runtime{}, &documents{root: t.TempDir()}
	r.observation = replacement.SourceObservation{Owner: source.ControllerOwner(), NativeID: "native-source", Provenance: domain.NativeThreadMain, State: ports.ChatControllerReady, ObservedAt: now}
	svc := replacement.New(replacement.Deps{Store: st, Runtime: r, Documents: d, Now: func() time.Time { return now }})
	return fixture{svc: svc, store: st, runtime: r, docs: d, source: source, project: p, request: replacement.Request{ProjectID: "demo", SourceSessionID: source.ID, OperationKey: "11111111-1111-4111-8111-111111111111"}}
}
func code(t *testing.T, err error, want string) {
	t.Helper()
	var api *apierr.Error
	if !errors.As(err, &api) || api.Code != want {
		t.Fatalf("error=%v want %s", err, want)
	}
}

func TestReplacementRejectsNonClaudeSourceBeforeSideEffects(t *testing.T) {
	for _, mode := range []domain.SessionMode{domain.SessionModeChat, domain.SessionModeTUI} {
		t.Run(string(mode), func(t *testing.T) {
			f := setup(t)
			ctx := context.Background()
			f.source.Harness, f.source.Mode = domain.HarnessCodex, mode
			if err := f.store.UpdateSession(ctx, f.source); err != nil {
				t.Fatal(err)
			}
			before, _, _ := f.store.GetSession(ctx, f.source.ID)
			op, err := f.svc.Accept(ctx, f.request)
			code(t, err, "ORCHESTRATOR_HANDOFF_SOURCE_UNSUPPORTED")
			if f.runtime.preflights != 0 || f.docs.calls != 0 || f.store.snapshotReads != 0 {
				t.Fatal("unsupported source reached provider/documents/history")
			}
			after, _, _ := f.store.GetSession(ctx, f.source.ID)
			if !reflect.DeepEqual(before, after) {
				t.Fatal("unsupported source was mutated")
			}
			got, err := f.svc.Get(ctx, op.ID)
			if err != nil || got.Phase != domain.ReplacementRejected || got.Error == nil || got.Error.Code != "ORCHESTRATOR_HANDOFF_SOURCE_UNSUPPORTED" {
				t.Fatalf("rejection not rereadable: %+v %v", got, err)
			}
		})
	}
}
func TestReplacementRejectsUnsupportedTargetAndHistory(t *testing.T) {
	for _, which := range []string{"target", "history"} {
		t.Run(which, func(t *testing.T) {
			f := setup(t)
			want := "ORCHESTRATOR_HANDOFF_HISTORY_UNSUPPORTED"
			if which == "target" {
				f.project.Config.Orchestrator.Harness = domain.HarnessCodex
				if err := f.store.UpsertProject(context.Background(), f.project); err != nil {
					t.Fatal(err)
				}
				want = "ORCHESTRATOR_HANDOFF_TARGET_UNSUPPORTED"
			} else {
				f.store.snapshot.NativeSessions[0].Harness = domain.HarnessCodex
			}
			_, err := f.svc.Accept(context.Background(), f.request)
			code(t, err, want)
			if f.runtime.preflights != 0 || f.docs.calls != 0 {
				t.Fatal("unsupported target/history reached launch preflight or documents")
			}
		})
	}
}
func TestReplacementAcceptedSurvivesCallerAndReplaysOriginalFacts(t *testing.T) {
	f := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	op, err := f.svc.Accept(ctx, f.request)
	if err != nil || op.Phase != domain.ReplacementAccepted || op.ID == "" {
		t.Fatalf("not durably accepted: %+v %v", op, err)
	}
	cancel()
	// A replay returns the same persisted facts even if today's config differs.
	f.project.Config.Orchestrator.Harness = domain.HarnessCodex
	if err := f.store.UpsertProject(context.Background(), f.project); err != nil {
		t.Fatal(err)
	}
	again, err := f.svc.Accept(context.Background(), f.request)
	if err != nil || again.ID != op.ID || again.Revision != op.Revision || f.runtime.preflights != 1 {
		t.Fatalf("replay re-ran preflight or changed identity: %+v %v", again, err)
	}
	got, err := f.svc.Get(context.Background(), op.ID)
	if err != nil || got.ID != op.ID || got.DeadlineAt.IsZero() {
		t.Fatalf("caller loss lost operation: %+v %v", got, err)
	}
	f.request.OperationKey = "22222222-2222-4222-8222-222222222222"
	_, err = f.svc.Accept(context.Background(), f.request)
	code(t, err, "ORCHESTRATOR_REPLACEMENT_IN_PROGRESS")
}
func TestReplacementValidationHasNoPersistentOrProviderEffects(t *testing.T) {
	f := setup(t)
	f.request.OperationKey = "not-a-uuid"
	_, err := f.svc.Accept(context.Background(), f.request)
	code(t, err, "ORCHESTRATOR_REPLACEMENT_REQUEST_INVALID")
	ops, err := f.store.ListActiveOrchestratorReplacements(context.Background())
	if err != nil || len(ops) != 0 || f.runtime.preflights != 0 || f.docs.calls != 0 || f.store.snapshotReads != 0 {
		t.Fatal("invalid request reached durable/provider side effect")
	}
}
