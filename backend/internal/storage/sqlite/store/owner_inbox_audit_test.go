package store_test

// Regression cases from the independent audit of the disabled H core.

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
	storeadapter "github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

type policyIntake struct {
	fixtureIntake
	observed *domain.InboxInputPlan
}

func (p policyIntake) EvaluateAndBind(ctx context.Context, plan domain.InboxInputPlan) (domain.InboxIntakeReceipt, error) {
	*p.observed = plan
	return p.fixtureIntake.EvaluateAndBind(ctx, plan)
}

// X1: the current admitted owner completes the immutable original batch plan.
func TestHOwnerInboxAudit169PolicyBumpCompletesFixedPlan(t *testing.T) {
	f := newInboxFixture(t)
	f.accept(t, "caller-1", "report-1")
	if _, err := f.core.ClaimBatch(t.Context(), f.plan()); err != nil {
		t.Fatal(err)
	}
	next := f.owner
	next.Epoch++
	next.PolicyRevision++
	if err := f.core.CompareAndSwapOwner(t.Context(), f.owner.Epoch, next); err != nil {
		t.Fatal(err)
	}
	if _, err := f.core.BindAcceptAndAckAO(t.Context(), f.input()); !errors.Is(err, domain.ErrOwnerInboxConflict) {
		t.Fatalf("stale owner: %v", err)
	}
	var observed domain.InboxInputPlan
	storeadapter.OwnerInboxTestParticipants(f.core, true, func(db gen.DBTX) (ports.OwnerInboxIntakeTransaction, ports.OwnerInboxMetadataTransaction) {
		return policyIntake{fixtureIntake: fixtureIntake{db: db}, observed: &observed}, fixtureMetadata{db: db}
	})
	p := f.input()
	p.Owner = next
	receipt, err := f.core.BindAcceptAndAckAO(t.Context(), p)
	if err != nil || receipt.OwnerEpoch != next.Epoch || observed.Owner.PolicyRevision != 2 || observed.BatchPolicyRevision != 1 {
		t.Fatalf("fixed policy completion: receipt=%+v intake=%+v err=%v", receipt, observed, err)
	}
	if replay, err := f.core.BindAcceptAndAckAO(t.Context(), p); err != nil || !reflect.DeepEqual(replay, receipt) {
		t.Fatalf("replay: %+v %v", replay, err)
	}
	inboxCount(t, f.db, `SELECT count(*) FROM owner_inbox_sources WHERE acknowledged_at IS NOT NULL`, 1)
	inboxCount(t, f.db, `SELECT count(*) FROM owner_inbox_batches WHERE policy_revision=1`, 1)
	inboxCount(t, f.db, `SELECT count(*) FROM owner_inbox_acceptances`, 1)
	inboxCount(t, f.db, `SELECT count(*) FROM fixture_s_intake`, 1)
	inboxCount(t, f.db, `SELECT count(*) FROM fixture_u1_metadata WHERE stage='bind'`, 1)
}

// X2: legacy coordinator claim in the same project after core AcceptReport.
func TestHOwnerInboxAudit169LegacyClaimAfterCoreAccept(t *testing.T) {
	f := newInboxFixture(t)
	f.accept(t, "caller-1", "report-1")
	legacy := domain.ReportRecord{ID: "legacy-1", SessionID: f.worker.ID, ProjectID: f.worker.ProjectID, State: domain.ReportCheckpoint, Note: "legacy", CreatedAt: testNow, AvailableAt: testNow, DeliveryState: domain.ReportPending, RepeatCount: 1}
	if _, err := f.s.CreateReport(t.Context(), legacy); err != nil {
		t.Fatal(err)
	}
	pending, err := f.s.ListPendingReportSchedule(t.Context(), 10)
	t.Logf("X2 legacy schedule sees %d pending (err=%v)", len(pending), err)
	claimed, err := f.s.ClaimPendingReportBatch(t.Context(), f.worker.ProjectID, "legacy-token", testNow.Add(time.Second))
	t.Logf("X2 legacy ClaimPendingReportBatch: claimed=%d err=%v", len(claimed), err)
	if err != nil {
		t.Errorf("X2 legacy project delivery blocked by H source: %v", err)
	}
	if len(claimed) != 1 || claimed[0].ID != "legacy-1" {
		t.Fatalf("legacy batch: %+v", claimed)
	}
	for _, r := range claimed {
		if r.ID == "report-1" {
			t.Errorf("X2 legacy coordinator claimed H source report-1")
		}
	}
}

// X3: direct wins, then piggyback carrying user text with a new key.
func TestHOwnerInboxAudit169PiggybackLosesUserText(t *testing.T) {
	f := newInboxFixture(t)
	f.participants(false, false)
	f.accept(t, "caller-1", "report-1")
	if _, err := f.core.ClaimBatch(t.Context(), f.plan()); err != nil {
		t.Fatal(err)
	}
	direct, err := f.core.BindAcceptAndAckAO(t.Context(), f.input())
	if err != nil {
		t.Fatal(err)
	}
	p := f.input()
	p.Key, p.TurnID, p.MessageID, p.ClientMessageID = "piggy", "piggy-turn", "piggy-msg", "piggy-client"
	u := "UNIQUE-HUMAN-TEXT-169"
	p.UserText = &u
	p.ExistingTurnID = p.TurnID
	p.Text = domain.AppendWorkerReportContext(u, p.SummaryText)
	if _, err := f.s.AppendUserMessage(t.Context(), p.Owner.ConversationID, p.Owner.SessionID, p.Owner.Generation, domain.ConversationMessage{ID: p.MessageID, Text: p.Text, ClientMessageID: p.ClientMessageID, Origin: domain.MessageOriginHuman}, p.TurnID, p.At); err != nil {
		t.Fatal(err)
	}
	got, err := f.core.BindAcceptAndAckAO(t.Context(), p)
	var n int
	_ = f.db.QueryRow(`SELECT count(*) FROM conversation_messages WHERE text LIKE '%UNIQUE-HUMAN-TEXT-169%'`).Scan(&n)
	t.Logf("X3 piggyback err=%v receipt.TurnID=%s (own=%s) equalToDirect=%v human-text rows=%d", err, got.TurnID, p.TurnID, reflect.DeepEqual(got, direct), n)
	if !errors.Is(err, domain.ErrOwnerInboxInputNotBound) || n != 1 {
		t.Fatalf("losing piggyback intent: receipt=%+v err=%v human rows=%d", got, err, n)
	}
	inboxCount(t, f.db, `SELECT count(*) FROM owner_inbox_actions WHERE action_key='piggy'`, 0)
	inboxCount(t, f.db, `SELECT count(*) FROM fixture_s_intake`, 1)
}

// X4: stale-epoch / candidate caller with a fresh key on an accepted batch.
func TestHOwnerInboxAudit169StaleEpochAliasWrites(t *testing.T) {
	f := newInboxFixture(t)
	f.participants(false, false)
	f.accept(t, "caller-1", "report-1")
	if _, err := f.core.ClaimBatch(t.Context(), f.plan()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.core.BindAcceptAndAckAO(t.Context(), f.input()); err != nil {
		t.Fatal(err)
	}
	next := f.owner
	next.Epoch++
	if err := f.core.CompareAndSwapOwner(t.Context(), f.owner.Epoch, next); err != nil {
		t.Fatal(err)
	}
	var before int
	_ = f.db.QueryRow(`SELECT count(*) FROM change_log`).Scan(&before)
	p := f.input()
	p.Key = "stale-alias"
	p.Owner.SessionID = f.worker.ID // not even an owner
	got, err := f.core.BindAcceptAndAckAO(t.Context(), p)
	var after, acts int
	_ = f.db.QueryRow(`SELECT count(*) FROM change_log`).Scan(&after)
	_ = f.db.QueryRow(`SELECT count(*) FROM owner_inbox_actions WHERE action_key='stale-alias'`).Scan(&acts)
	t.Logf("X4 stale/non-owner alias err=%v epoch=%d actions=%d cdc+%d", err, got.OwnerEpoch, acts, after-before)
	if !errors.Is(err, domain.ErrOwnerInboxConflict) || acts != 0 || after != before {
		t.Fatalf("non-owner alias wrote durable facts: err=%v actions=%d cdc+%d", err, acts, after-before)
	}
}

// X5: concurrency — many AcceptReport same key, many ClaimBatch aliases, many binds.
func TestHOwnerInboxAudit169Concurrency(t *testing.T) {
	f := newInboxFixture(t)
	f.participants(false, false)
	const N = 16
	var wg sync.WaitGroup
	recs := make([]domain.InboxReceipt, N)
	errs := make([]error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			recs[i], errs[i] = f.core.AcceptReport(context.Background(), domain.InboxReportIntent{Key: "same", Report: domain.ReportRecord{ID: fmt.Sprintf("r-%d", i), SessionID: f.worker.ID, ProjectID: f.worker.ProjectID, State: domain.ReportCheckpoint, Note: "n", CreatedAt: testNow, AvailableAt: testNow, DeliveryState: domain.ReportPending, RepeatCount: 1}})
		}(i)
	}
	wg.Wait()
	for i := range errs {
		if errs[i] != nil || !reflect.DeepEqual(recs[i], recs[0]) {
			t.Fatalf("accept %d: %v %+v", i, errs[i], recs[i])
		}
	}
	inboxCount(t, f.db, `SELECT count(*) FROM owner_inbox_sources`, 1)
	inboxCount(t, f.db, `SELECT count(*) FROM fixture_u1_metadata WHERE stage='accept'`, 1)
	rid := recs[0].ReportID
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := f.plan()
			p.Key = fmt.Sprintf("claim-%d", i)
			p.Members = []domain.InboxSourceVersion{{ReportID: rid, Version: 1}}
			recs[i], errs[i] = f.core.ClaimBatch(context.Background(), p)
		}(i)
	}
	wg.Wait()
	for i := range errs {
		if errs[i] != nil || !reflect.DeepEqual(recs[i], recs[0]) {
			t.Fatalf("claim %d: %v %+v vs %+v", i, errs[i], recs[i], recs[0])
		}
	}
	inboxCount(t, f.db, `SELECT count(*) FROM owner_inbox_batches`, 1)
	inboxCount(t, f.db, `SELECT count(*) FROM fixture_u1_metadata WHERE stage='claim'`, 1)
	batch := recs[0].BatchKey
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := f.input()
			p.BatchKey = batch
			p.Key = fmt.Sprintf("bind-%d", i)
			p.TurnID, p.MessageID = fmt.Sprintf("t-%d", i), fmt.Sprintf("m-%d", i)
			recs[i], errs[i] = f.core.BindAcceptAndAckAO(context.Background(), p)
		}(i)
	}
	wg.Wait()
	for i := range errs {
		if errs[i] != nil || !reflect.DeepEqual(recs[i], recs[0]) {
			t.Fatalf("bind %d: %v", i, errs[i])
		}
	}
	inboxCount(t, f.db, `SELECT count(*) FROM owner_inbox_acceptances`, 1)
	inboxCount(t, f.db, `SELECT count(*) FROM fixture_s_intake`, 1)
	inboxCount(t, f.db, `SELECT count(*) FROM fixture_u1_metadata WHERE stage='bind'`, 1)
	inboxCount(t, f.db, `SELECT count(*) FROM conversation_turns WHERE conversation_id='inbox-context'`, 1)
	inboxCount(t, f.db, `SELECT count(*) FROM owner_inbox_sources WHERE acknowledged_at IS NOT NULL`, 1)
}

type storeReentry struct {
	s interface {
		CreateReport(context.Context, domain.ReportRecord) (domain.ReportRecord, error)
	}
	w domain.SessionRecord
}

func (p storeReentry) Enlist(ctx context.Context, _ domain.InboxMetadataPlan) ([]string, error) {
	_, err := p.s.CreateReport(ctx, domain.ReportRecord{ID: "reenter", SessionID: p.w.ID, ProjectID: p.w.ProjectID, State: domain.ReportCheckpoint, Note: "x", CreatedAt: testNow, AvailableAt: testNow, DeliveryState: domain.ReportPending, RepeatCount: 1})
	return nil, err
}

// X6: participant reenters a public Store writer (not the core) — deadlock vs clean reject.
func TestHOwnerInboxAudit169ParticipantStoreReentry(t *testing.T) {
	f := newInboxFixture(t)
	storeadapter.OwnerInboxTestParticipants(f.core, true, func(db gen.DBTX) (ports.OwnerInboxIntakeTransaction, ports.OwnerInboxMetadataTransaction) {
		return fixtureIntake{db: db}, storeReentry{s: f.s, w: f.worker}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	start := time.Now()
	_, err := f.core.AcceptReport(ctx, domain.InboxReportIntent{Key: "k", Report: domain.ReportRecord{ID: "r", SessionID: f.worker.ID, ProjectID: f.worker.ProjectID, State: domain.ReportCheckpoint, Note: "n", CreatedAt: testNow, AvailableAt: testNow, DeliveryState: domain.ReportPending, RepeatCount: 1}})
	t.Logf("X6 store reentry err=%v after %s", err, time.Since(start).Round(time.Millisecond))
	if !errors.Is(err, domain.ErrOwnerInboxConflict) || time.Since(start) >= time.Second {
		t.Fatalf("Store reentry did not fail immediately: %v", err)
	}
	inboxCount(t, f.db, `SELECT count(*) FROM owner_inbox_sources`, 0)
}

type panicMeta struct{}

func (panicMeta) Enlist(context.Context, domain.InboxMetadataPlan) ([]string, error) { panic("boom") }

// X7: panic inside participant must roll back and release writeMu.
func TestHOwnerInboxAudit169ParticipantPanic(t *testing.T) {
	f := newInboxFixture(t)
	storeadapter.OwnerInboxTestParticipants(f.core, true, func(db gen.DBTX) (ports.OwnerInboxIntakeTransaction, ports.OwnerInboxMetadataTransaction) {
		return fixtureIntake{db: db}, panicMeta{}
	})
	func() {
		defer func() {
			if r := recover(); r != "boom" {
				t.Errorf("unexpected panic: %v", r)
			}
		}()
		_, _ = f.core.AcceptReport(t.Context(), domain.InboxReportIntent{Key: "k", Report: domain.ReportRecord{ID: "r", SessionID: f.worker.ID, ProjectID: f.worker.ProjectID, State: domain.ReportCheckpoint, Note: "n", CreatedAt: testNow, AvailableAt: testNow, DeliveryState: domain.ReportPending, RepeatCount: 1}})
	}()
	inboxCount(t, f.db, `SELECT count(*) FROM owner_inbox_sources`, 0)
	f.participants(false, false)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if _, err := f.core.AcceptReport(ctx, domain.InboxReportIntent{Key: "k2", Report: domain.ReportRecord{ID: "r2", SessionID: f.worker.ID, ProjectID: f.worker.ProjectID, State: domain.ReportCheckpoint, Note: "n", CreatedAt: testNow, AvailableAt: testNow, DeliveryState: domain.ReportPending, RepeatCount: 1}}); err != nil {
		t.Fatalf("X7 writer not released after panic: %v", err)
	}
}

// X8: canceled context inside S participant after writes.
type cancelIntake struct {
	db     gen.DBTX
	cancel context.CancelFunc
}

func (p cancelIntake) EvaluateAndBind(ctx context.Context, plan domain.InboxInputPlan) (domain.InboxIntakeReceipt, error) {
	if _, err := p.db.ExecContext(ctx, `INSERT INTO fixture_s_intake(input_id,receipt_ref) VALUES(?,?)`, plan.TurnID, "x"); err != nil {
		return domain.InboxIntakeReceipt{}, err
	}
	p.cancel()
	return domain.InboxIntakeReceipt{InputID: plan.TurnID, ReceiptRef: "x"}, nil
}

func TestHOwnerInboxAudit169CancelMidTransaction(t *testing.T) {
	f := newInboxFixture(t)
	f.accept(t, "caller-1", "report-1")
	if _, err := f.core.ClaimBatch(t.Context(), f.plan()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	storeadapter.OwnerInboxTestParticipants(f.core, false, func(db gen.DBTX) (ports.OwnerInboxIntakeTransaction, ports.OwnerInboxMetadataTransaction) {
		return cancelIntake{db: db, cancel: cancel}, nil
	})
	_, err := f.core.BindAcceptAndAckAO(ctx, f.input())
	t.Logf("X8 cancel err=%v", err)
	var acc, sIn int
	_ = f.db.QueryRow(`SELECT count(*) FROM owner_inbox_acceptances`).Scan(&acc)
	_ = f.db.QueryRow(`SELECT count(*) FROM fixture_s_intake`).Scan(&sIn)
	var state string
	_ = f.db.QueryRow(`SELECT CASE WHEN acknowledged_at IS NULL THEN 'pending' ELSE 'acknowledged' END FROM owner_inbox_sources WHERE report_id='report-1'`).Scan(&state)
	if (err == nil) != (acc == 1) || (acc == 1) != (sIn == 1) {
		t.Errorf("X8 partial commit: err=%v acceptances=%d s=%d state=%s", err, acc, sIn, state)
	}
	t.Logf("X8 acceptances=%d s=%d state=%s", acc, sIn, state)
	if !errors.Is(err, context.Canceled) || acc != 0 || sIn != 0 {
		t.Fatalf("canceled transaction committed: err=%v acceptances=%d intake=%d", err, acc, sIn)
	}
	inboxCount(t, f.db, `SELECT count(*) FROM owner_inbox_sources WHERE acknowledged_at IS NULL`, 1)
}

// X9: legacy delete of worker session that has an H source.
func TestHOwnerInboxAudit169DeleteWorkerWithSource(t *testing.T) {
	f := newInboxFixture(t)
	f.accept(t, "caller-1", "report-1")
	if _, err := f.db.Exec(`UPDATE sessions SET is_terminated=1 WHERE id=?`, f.worker.ID); err != nil {
		t.Fatal(err)
	}
	ok, err := f.s.DeleteSession(t.Context(), f.worker.ID)
	t.Logf("X9 DeleteSession(worker with H source) ok=%v err=%v", ok, err)
	if err != nil || ok {
		t.Fatalf("terminated worker must retain legacy seed-delete no-op: deleted=%t err=%v", ok, err)
	}
	inboxCount(t, f.db, `SELECT count(*) FROM owner_inbox_sources WHERE acknowledged_at IS NULL`, 1)
}

// X10: CDC row shape.
func TestHOwnerInboxAudit169CDCShape(t *testing.T) {
	f := newInboxFixture(t)
	f.accept(t, "caller-1", "report-1")
	rows, err := f.db.Query(`SELECT event_type, coalesce(session_id,'<NULL>'), payload FROM change_log WHERE payload LIKE '%ownerInboxChanged%'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		count++
		var a, b, c string
		_ = rows.Scan(&a, &b, &c)
		t.Logf("X10 cdc event=%s session=%s payload=%s", a, b, c)
		if a != "session_updated" {
			t.Errorf("unexpected CDC kind: %s", a)
		}
	}
	if err := rows.Err(); err != nil || count != 1 {
		t.Fatalf("CDC count=%d err=%v", count, err)
	}
}
