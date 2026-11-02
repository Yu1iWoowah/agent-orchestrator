package store_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
	storeadapter "github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

type inboxFixture struct {
	s      *sqlite.Store
	db     *sql.DB
	core   *storeadapter.OwnerInboxCore
	worker domain.SessionRecord
	owner  domain.InboxOwner
}

func newInboxFixture(t *testing.T) inboxFixture {
	t.Helper()
	s, db, worker := settingsStartFixture(t, "")
	rec := sampleRecord(string(worker.ProjectID))
	rec.Kind = domain.KindOrchestrator
	rec.Mode = domain.SessionModeChat
	rec.Metadata.ControllerGeneration = "owner-generation"
	rec.Metadata.RuntimeLaunchID = "owner-generation"
	orch, err := s.CreateSession(t.Context(), rec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO conversations(id,scope,project_id,current_session_id,active_branch_id,created_at,updated_at) VALUES('inbox-context','project',?,?,'inbox-branch',?,?)`, worker.ProjectID, orch.ID, testNow, testNow); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE fixture_s_intake(input_id TEXT PRIMARY KEY,receipt_ref TEXT NOT NULL);
 CREATE TABLE fixture_u1_metadata(source_id TEXT NOT NULL,source_version INTEGER NOT NULL,stage TEXT NOT NULL,PRIMARY KEY(source_id,source_version,stage));`); err != nil {
		t.Fatal(err)
	}
	core := storeadapter.NewOwnerInboxCore(s)
	owner := domain.InboxOwner{ProjectID: worker.ProjectID, SessionID: orch.ID, Epoch: 1, ConversationID: "inbox-context", BranchID: "inbox-branch", Generation: "owner-generation", PolicyRevision: 1}
	if err := core.CompareAndSwapOwner(t.Context(), 0, owner); err != nil {
		t.Fatal(err)
	}
	return inboxFixture{s: s, db: db, core: core, worker: worker, owner: owner}
}

func (f inboxFixture) accept(t *testing.T, key, id string) domain.InboxReceipt {
	t.Helper()
	r, err := f.core.AcceptReport(t.Context(), domain.InboxReportIntent{Key: key, Report: domain.ReportRecord{ID: id, SessionID: f.worker.ID, ProjectID: f.worker.ProjectID, State: domain.ReportCheckpoint, Note: "work recorded", CreatedAt: testNow, AvailableAt: testNow, DeliveryState: domain.ReportPending, RepeatCount: 1}})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (f inboxFixture) plan() domain.InboxBatchPlan {
	return domain.InboxBatchPlan{ProjectID: f.worker.ProjectID, Key: "batch-1", Owner: f.owner, Members: []domain.InboxSourceVersion{{ReportID: "report-1", Version: 1}}, TextHash: storeadapter.InboxTextHash("report summary")}
}

func (f inboxFixture) input() domain.InboxInputPlan {
	return domain.InboxInputPlan{
		ProjectID: f.worker.ProjectID, Key: "bind-1", BatchKey: "batch-1", Owner: f.owner,
		TurnID: "inbox-turn", MessageID: "inbox-message", ClientMessageID: "inbox-client",
		Text: "report summary", SummaryText: "report summary", At: testNow,
		SettingsRef: "fixture-qualified-settings", SettingsRevision: 1,
		RuntimeQualificationRef: "fixture-runtime", HeadRevision: 1, LedgerRevision: 1,
		SettingsOwner: domain.AgentSettingsOwner{SessionID: f.owner.SessionID, AccountID: "fixture-account", ControllerGeneration: f.owner.Generation, ProviderConversationID: "fixture-native", BranchID: f.owner.BranchID},
	}
}

// Fixtures write real rows using the same native transaction. They do not
// implement S's eligibility or claim production support for outbound I/O.
type fixtureIntake struct {
	db   gen.DBTX
	fail bool
	hold bool
}

func (p fixtureIntake) EvaluateAndBind(ctx context.Context, plan domain.InboxInputPlan) (domain.InboxIntakeReceipt, error) {
	_, err := p.db.ExecContext(ctx, `INSERT INTO fixture_s_intake(input_id,receipt_ref) VALUES(?,?)`, plan.TurnID, "fixture-intake:"+plan.TurnID)
	if err != nil {
		return domain.InboxIntakeReceipt{}, err
	}
	if p.hold {
		q := gen.New(p.db)
		if err := q.InsertInputCustodyAction(ctx, gen.InsertInputCustodyActionParams{ActionKey: "fixture-register:" + plan.TurnID, TurnID: plan.TurnID, Kind: "register", TargetOwner: "S-fixture", CreatedAt: plan.At}); err != nil {
			return domain.InboxIntakeReceipt{}, err
		}
		if err := q.InsertInputCustodyAction(ctx, gen.InsertInputCustodyActionParams{ActionKey: "fixture-hold:" + plan.TurnID, TurnID: plan.TurnID, Kind: "hold", ExpectedOwner: "S-fixture", ExpectedRevision: 1, HoldKey: "fixture-head", HoldOwner: "S-fixture", Reason: "head_blocked", CreatedAt: plan.At}); err != nil {
			return domain.InboxIntakeReceipt{}, err
		}
	}
	if p.fail {
		return domain.InboxIntakeReceipt{}, errors.New("fixture S failure after write")
	}
	r := domain.InboxIntakeReceipt{InputID: plan.TurnID, ReceiptRef: "fixture-intake:" + plan.TurnID}
	if p.hold {
		r.SafeReason = "queue_head_unresolved"
	}
	return r, nil
}

type fixtureMetadata struct {
	db   gen.DBTX
	fail bool
}

func (p fixtureMetadata) Enlist(ctx context.Context, plan domain.InboxMetadataPlan) ([]string, error) {
	var refs []string
	for _, source := range plan.Sources {
		if _, err := p.db.ExecContext(ctx, `INSERT INTO fixture_u1_metadata(source_id,source_version,stage) VALUES(?,?,?)`, source.ReportID, source.Version, plan.Stage); err != nil {
			return nil, err
		}
		refs = append(refs, fmt.Sprintf("fixture:%s:%d:%s", source.ReportID, source.Version, plan.Stage))
	}
	if p.fail {
		return nil, errors.New("fixture U1 failure after write")
	}
	return refs, nil
}
func (f inboxFixture) participants(sFail, mFail bool) {
	storeadapter.OwnerInboxTestParticipants(f.core, true, func(db gen.DBTX) (ports.OwnerInboxIntakeTransaction, ports.OwnerInboxMetadataTransaction) {
		return fixtureIntake{db: db, fail: sFail}, fixtureMetadata{db: db, fail: mFail}
	})
}
func inboxCount(t *testing.T, db *sql.DB, query string, want int) {
	t.Helper()
	var n int
	if err := db.QueryRow(query).Scan(&n); err != nil || n != want {
		t.Fatalf("%s: count=%d want=%d err=%v", query, n, want, err)
	}
}

func TestHOwnerInboxSourceGuard(t *testing.T) {
	f := newInboxFixture(t)
	f.accept(t, "caller-1", "report-1")
	if _, err := f.db.Exec(`UPDATE reports SET note='changed after intake' WHERE id='report-1'`); err == nil {
		t.Fatal("accepted source content was rewritten")
	}
}

func TestHOwnerInboxRegisterSentinel(t *testing.T) {
	s, db := inputReplacementFixture(t, "ordinary", false)
	if _, err := db.Exec(`UPDATE conversation_messages SET rowid=-1 WHERE id='message-protected'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RegisterInputCustody(t.Context(), "protected", "owner", "sentinel", testNow); !errors.Is(err, domain.ErrInputCustodyStorageIncompatible) {
		t.Fatalf("sentinel register: %v", err)
	}
}

func TestHOwnerInboxAcceptReplayAndRoutingUnavailable(t *testing.T) {
	f := newInboxFixture(t)
	if _, err := f.db.Exec(`DELETE FROM project_orchestrator_owners`); err != nil {
		t.Fatal(err)
	}
	one := f.accept(t, "caller-1", "report-1")
	two := f.accept(t, "caller-1", "generated-on-retry")
	if !reflect.DeepEqual(one, two) || one.SafeReason != "routing_unavailable" {
		t.Fatalf("replay/routing: %+v %+v", one, two)
	}
	_, err := f.core.ClaimBatch(t.Context(), f.plan())
	if !errors.Is(err, domain.ErrOwnerRoutingUnavailable) {
		t.Fatalf("unresolved claim: %v", err)
	}
	inboxCount(t, f.db, `SELECT count(*) FROM reports`, 1)
	inboxCount(t, f.db, `SELECT count(*) FROM owner_inbox_acceptances`, 0)
}

func TestHOwnerInboxAtomicParticipants(t *testing.T) {
	for _, stage := range []string{"accept", "claim", "S-bind", "U1-bind", "H-commit"} {
		t.Run(stage, func(t *testing.T) {
			f := newInboxFixture(t)
			if stage == "accept" {
				f.participants(false, true)
				_, err := f.core.AcceptReport(t.Context(), domain.InboxReportIntent{Key: "caller-1", Report: domain.ReportRecord{ID: "report-1", SessionID: f.worker.ID, ProjectID: f.worker.ProjectID, CreatedAt: testNow, AvailableAt: testNow, DeliveryState: domain.ReportPending, RepeatCount: 1, Message: "source"}})
				if err == nil {
					t.Fatal("metadata failure committed")
				}
				inboxCount(t, f.db, `SELECT count(*) FROM reports`, 0)
				inboxCount(t, f.db, `SELECT count(*) FROM fixture_u1_metadata`, 0)
				return
			}
			f.accept(t, "caller-1", "report-1")
			if stage == "claim" {
				f.participants(false, true)
				if _, err := f.core.ClaimBatch(t.Context(), f.plan()); err == nil {
					t.Fatal("metadata failure committed")
				}
				inboxCount(t, f.db, `SELECT count(*) FROM owner_inbox_batches`, 0)
				inboxCount(t, f.db, `SELECT count(*) FROM owner_inbox_members`, 0)
				inboxCount(t, f.db, `SELECT count(*) FROM fixture_u1_metadata`, 0)
				return
			}
			if _, err := f.core.ClaimBatch(t.Context(), f.plan()); err != nil {
				t.Fatal(err)
			}
			f.participants(stage == "S-bind", stage == "U1-bind")
			if stage == "H-commit" {
				if _, err := f.db.Exec(`CREATE TRIGGER fixture_h_failure BEFORE UPDATE ON reports WHEN NEW.delivery_state='acknowledged' BEGIN SELECT RAISE(ABORT,'fixture H failure after participants');END;`); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.core.BindAcceptAndAckAO(t.Context(), f.input()); err == nil {
				t.Fatal("participant/core failure committed")
			}
			for _, table := range []string{"fixture_s_intake", "fixture_u1_metadata", "owner_inbox_acceptances"} {
				inboxCount(t, f.db, `SELECT count(*) FROM `+table, 0)
			}
			inboxCount(t, f.db, `SELECT count(*) FROM conversation_turns WHERE id='inbox-turn'`, 0)
			inboxCount(t, f.db, `SELECT count(*) FROM reports WHERE delivery_state='pending'`, 1)
			if _, found, err := f.core.GetByActionKey(t.Context(), f.worker.ProjectID, "bind-1"); err != nil || found {
				t.Fatalf("uncommitted receipt found=%v err=%v", found, err)
			}
		})
	}
}

func TestHOwnerInboxDirectPiggybackReplayAndOwnerEpoch(t *testing.T) {
	f := newInboxFixture(t)
	f.participants(false, false)
	f.accept(t, "caller-1", "report-1")
	if _, err := f.core.ClaimBatch(t.Context(), f.plan()); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make([]domain.InboxReceipt, 2)
	errs := make([]error, 2)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := f.input()
			p.Key = fmt.Sprintf("path-%d", i)
			p.TurnID = fmt.Sprintf("path-turn-%d", i)
			p.MessageID = fmt.Sprintf("path-message-%d", i)
			if i == 1 {
				userText := "a concurrent user instruction"
				p.UserText = &userText
			}
			results[i], errs[i] = f.core.BindAcceptAndAckAO(t.Context(), p)
		}(i)
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil || !reflect.DeepEqual(results[0], results[1]) {
		t.Fatalf("race: receipts=%+v errors=%v", results, errs)
	}
	inboxCount(t, f.db, `SELECT count(*) FROM owner_inbox_acceptances`, 1)
	inboxCount(t, f.db, `SELECT count(*) FROM fixture_s_intake`, 1)
	inboxCount(t, f.db, `SELECT count(*) FROM fixture_u1_metadata WHERE stage='bind'`, 1)
	next := f.owner
	next.Epoch++
	if err := f.core.CompareAndSwapOwner(t.Context(), f.owner.Epoch, next); err != nil {
		t.Fatal(err)
	}
	prior, found, err := f.core.GetByActionKey(t.Context(), f.worker.ProjectID, results[0].Key)
	if err != nil || !found || !reflect.DeepEqual(prior, results[0]) {
		t.Fatalf("epoch lost result: %+v %v %v", prior, found, err)
	}
	p := f.input()
	p.Key = results[0].Key
	if p.Key == "path-1" {
		userText := "a concurrent user instruction"
		p.UserText = &userText
	}
	if replay, err := f.core.BindAcceptAndAckAO(t.Context(), p); err != nil || !reflect.DeepEqual(replay, prior) {
		t.Fatalf("lost-response replay: %+v %v", replay, err)
	}
}

func TestHOwnerInboxMissingParticipantAndStaleAdmission(t *testing.T) {
	f := newInboxFixture(t)
	f.accept(t, "caller-1", "report-1")
	p := f.plan()
	p.Owner.PolicyRevision++
	if _, err := f.core.ClaimBatch(t.Context(), p); !errors.Is(err, domain.ErrOwnerInboxConflict) {
		t.Fatalf("stale policy: %v", err)
	}
	if _, err := f.core.ClaimBatch(t.Context(), f.plan()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.core.BindAcceptAndAckAO(t.Context(), f.input()); !errors.Is(err, domain.ErrOwnerInboxParticipantMissing) {
		t.Fatalf("missing gate: %v", err)
	}
	inboxCount(t, f.db, `SELECT count(*) FROM owner_inbox_acceptances`, 0)
	f.participants(false, false)
	next := f.owner
	next.Epoch++
	if err := f.core.CompareAndSwapOwner(t.Context(), f.owner.Epoch, next); err != nil {
		t.Fatal(err)
	}
	if _, err := f.core.BindAcceptAndAckAO(t.Context(), f.input()); !errors.Is(err, domain.ErrOwnerInboxConflict) {
		t.Fatalf("old owner accepted: %v", err)
	}
	inboxCount(t, f.db, `SELECT count(*) FROM conversation_turns WHERE id='inbox-turn'`, 0)
}

func TestHOwnerInboxFixedPlansAndImmutableReceipts(t *testing.T) {
	f := newInboxFixture(t)
	f.participants(false, false)
	accepted := f.accept(t, "caller-1", "report-1")
	if _, err := f.core.AcceptReport(t.Context(), domain.InboxReportIntent{Key: "caller-1", Report: domain.ReportRecord{ID: "different", SessionID: f.worker.ID, ProjectID: f.worker.ProjectID, State: domain.ReportCheckpoint, Note: "different payload", CreatedAt: testNow, AvailableAt: testNow, DeliveryState: domain.ReportPending, RepeatCount: 1}}); !errors.Is(err, domain.ErrOwnerInboxConflict) {
		t.Fatalf("caller key reused with different content: %v", err)
	}
	claimed, err := f.core.ClaimBatch(t.Context(), f.plan())
	if err != nil {
		t.Fatal(err)
	}
	alias := f.plan()
	alias.Key = "competing-claim"
	got, err := f.core.ClaimBatch(t.Context(), alias)
	if err != nil || !reflect.DeepEqual(got, claimed) {
		t.Fatalf("claim alias: %+v %v", got, err)
	}
	if got, found, err := f.core.GetByActionKey(t.Context(), f.worker.ProjectID, alias.Key); err != nil || !found || !reflect.DeepEqual(got, claimed) {
		t.Fatalf("lost claim response: %+v %v %v", got, found, err)
	}
	alias.TextHash = storeadapter.InboxTextHash("changed plan")
	if _, err := f.core.ClaimBatch(t.Context(), alias); !errors.Is(err, domain.ErrOwnerInboxConflict) {
		t.Fatalf("fixed plan rewritten: %v", err)
	}
	bound, err := f.core.BindAcceptAndAckAO(t.Context(), f.input())
	if err != nil {
		t.Fatal(err)
	}
	p := f.input()
	p.Key = "competing-bind"
	if got, err := f.core.BindAcceptAndAckAO(t.Context(), p); err != nil || !reflect.DeepEqual(got, bound) {
		t.Fatalf("bind alias: %+v %v", got, err)
	}
	if got, found, err := f.core.GetByActionKey(t.Context(), f.worker.ProjectID, p.Key); err != nil || !found || !reflect.DeepEqual(got, bound) {
		t.Fatalf("lost bind response: %+v %v %v", got, found, err)
	}
	p.Key = "changed-bind"
	p.SummaryText = "changed text"
	if _, err := f.core.BindAcceptAndAckAO(t.Context(), p); !errors.Is(err, domain.ErrOwnerInboxConflict) {
		t.Fatalf("canonical input rewritten: %v", err)
	}
	for _, statement := range []string{
		`UPDATE owner_inbox_actions SET receipt_json='{}'`,
		`DELETE FROM owner_inbox_actions`,
		`UPDATE owner_inbox_members SET source_version=2`,
		`UPDATE owner_inbox_sources SET source_version=2`,
		`UPDATE reports SET delivery_state='pending' WHERE id='report-1'`,
		`INSERT OR REPLACE INTO reports SELECT * FROM reports WHERE id='report-1'`,
	} {
		if _, err := f.db.Exec(statement); err == nil {
			t.Fatalf("immutable fact changed: %s", statement)
		}
	}
	if got, found, err := f.core.GetByActionKey(t.Context(), f.worker.ProjectID, accepted.Key); err != nil || !found || !reflect.DeepEqual(got, accepted) {
		t.Fatalf("original source receipt: %+v %v %v", got, found, err)
	}
}

func TestHOwnerInboxCandidateAndGenerationCannotClaim(t *testing.T) {
	f := newInboxFixture(t)
	f.accept(t, "caller-1", "report-1")
	p := f.plan()
	p.Owner.SessionID = f.worker.ID
	if _, err := f.core.ClaimBatch(t.Context(), p); !errors.Is(err, domain.ErrOwnerInboxConflict) {
		t.Fatalf("non-owner claim: %v", err)
	}
	next := f.owner
	next.Epoch++
	next.SessionID = f.worker.ID
	if err := f.core.CompareAndSwapOwner(t.Context(), 1, next); !errors.Is(err, domain.ErrOwnerRoutingUnavailable) {
		t.Fatalf("worker promoted without projection: %v", err)
	}
	if _, err := f.db.Exec(`UPDATE sessions SET controller_generation='different-launch' WHERE id=?`, f.owner.SessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.core.ClaimBatch(t.Context(), f.plan()); !errors.Is(err, domain.ErrOwnerRoutingUnavailable) {
		t.Fatalf("stale generation claimed: %v", err)
	}
	inboxCount(t, f.db, `SELECT count(*) FROM owner_inbox_members`, 0)
}

func TestHOwnerInboxDeclaredMetadataCannotDisappear(t *testing.T) {
	f := newInboxFixture(t)
	storeadapter.OwnerInboxTestParticipants(f.core, true, func(db gen.DBTX) (ports.OwnerInboxIntakeTransaction, ports.OwnerInboxMetadataTransaction) {
		return fixtureIntake{db: db}, nil
	})
	_, err := f.core.AcceptReport(t.Context(), domain.InboxReportIntent{Key: "caller", Report: domain.ReportRecord{ID: "report", SessionID: f.worker.ID, ProjectID: f.worker.ProjectID, Message: "source", CreatedAt: testNow, AvailableAt: testNow, DeliveryState: domain.ReportPending, RepeatCount: 1}})
	if !errors.Is(err, domain.ErrOwnerInboxParticipantMissing) {
		t.Fatalf("advertised metadata silently disabled: %v", err)
	}
	inboxCount(t, f.db, `SELECT count(*) FROM reports`, 0)
}

func TestHOwnerInboxHeldIntakeWithoutU1(t *testing.T) {
	f := newInboxFixture(t)
	f.accept(t, "caller-1", "report-1")
	if _, err := f.core.ClaimBatch(t.Context(), f.plan()); err != nil {
		t.Fatal(err)
	}
	storeadapter.OwnerInboxTestParticipants(f.core, false, func(db gen.DBTX) (ports.OwnerInboxIntakeTransaction, ports.OwnerInboxMetadataTransaction) {
		return fixtureIntake{db: db, hold: true}, nil
	})
	r, err := f.core.BindAcceptAndAckAO(t.Context(), f.input())
	if err != nil || r.SafeReason != "queue_head_unresolved" {
		t.Fatalf("durable held intake: %+v %v", r, err)
	}
	inboxCount(t, f.db, `SELECT count(*) FROM conversation_turns WHERE id='inbox-turn' AND state='held'`, 1)
	inboxCount(t, f.db, `SELECT count(*) FROM conversation_input_holds WHERE turn_id='inbox-turn' AND active=1`, 1)
	inboxCount(t, f.db, `SELECT count(*) FROM reports WHERE delivery_state='acknowledged'`, 1)
	if err := f.s.InputCustodyStorageCompatibility(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestHOwnerInboxBindsExistingPiggybackInput(t *testing.T) {
	f := newInboxFixture(t)
	f.accept(t, "caller-1", "report-1")
	if _, err := f.core.ClaimBatch(t.Context(), f.plan()); err != nil {
		t.Fatal(err)
	}
	p := f.input()
	p.ExistingTurnID = p.TurnID
	userText := "a real user instruction"
	p.UserText = &userText
	p.Text = domain.AppendWorkerReportContext(userText, p.SummaryText)
	if _, err := f.s.AppendUserMessage(t.Context(), p.Owner.ConversationID, p.Owner.SessionID, p.Owner.Generation, domain.ConversationMessage{ID: p.MessageID, Text: p.Text, ClientMessageID: p.ClientMessageID, Origin: domain.MessageOriginHuman}, p.TurnID, p.At); err != nil {
		t.Fatal(err)
	}
	f.participants(false, false)
	r, err := f.core.BindAcceptAndAckAO(t.Context(), p)
	if err != nil || r.TurnID != p.TurnID {
		t.Fatalf("piggyback binding: %+v %v", r, err)
	}
	inboxCount(t, f.db, `SELECT count(*) FROM conversation_turns WHERE conversation_id='inbox-context'`, 1)
	inboxCount(t, f.db, `SELECT count(*) FROM conversation_messages WHERE conversation_id='inbox-context' AND origin='human'`, 1)
}

// An adapter receives a transaction-marked context, never a reentrant core lane.
type reentrantInboxMetadata struct {
	core *storeadapter.OwnerInboxCore
	plan domain.InboxBatchPlan
}

func (p reentrantInboxMetadata) Enlist(ctx context.Context, _ domain.InboxMetadataPlan) ([]string, error) {
	_, err := p.core.ClaimBatch(ctx, p.plan)
	return nil, err
}
func TestHOwnerInboxParticipantCannotReenterCoordinator(t *testing.T) {
	f := newInboxFixture(t)
	f.accept(t, "caller-1", "report-1")
	if _, err := f.core.ClaimBatch(t.Context(), f.plan()); err != nil {
		t.Fatal(err)
	}
	storeadapter.OwnerInboxTestParticipants(f.core, true, func(db gen.DBTX) (ports.OwnerInboxIntakeTransaction, ports.OwnerInboxMetadataTransaction) {
		return fixtureIntake{db: db}, reentrantInboxMetadata{core: f.core, plan: f.plan()}
	})
	if _, err := f.core.BindAcceptAndAckAO(t.Context(), f.input()); !errors.Is(err, domain.ErrOwnerInboxConflict) {
		t.Fatalf("participant reentry: %v", err)
	}
	inboxCount(t, f.db, `SELECT count(*) FROM fixture_s_intake`, 0)
	inboxCount(t, f.db, `SELECT count(*) FROM owner_inbox_acceptances`, 0)
}
