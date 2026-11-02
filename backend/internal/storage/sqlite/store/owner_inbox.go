package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// OwnerInboxCore is deliberately not installed in the daemon. S-G0 must supply
// real transaction-bound intake and outbound adapters before production cutover.
type OwnerInboxCore struct {
	store *Store
	// Only adapters in this package can bind participants to the SQL transaction.
	// Domain callers never receive SQL, commit/rollback or a callback into H.
	participants    func(gen.DBTX) (ports.OwnerInboxIntakeTransaction, ports.OwnerInboxMetadataTransaction)
	metadataEnabled bool
}

var _ ports.OwnerInbox = (*OwnerInboxCore)(nil)

type ownerInboxTransactionKey struct{}

// NewOwnerInboxCore creates an unwired core with no S or U1 participants.
func NewOwnerInboxCore(s *Store) *OwnerInboxCore { return &OwnerInboxCore{store: s} }

func (c *OwnerInboxCore) transactionParticipants(db gen.DBTX) (ports.OwnerInboxIntakeTransaction, ports.OwnerInboxMetadataTransaction, error) {
	var intake ports.OwnerInboxIntakeTransaction
	var metadata ports.OwnerInboxMetadataTransaction
	if c.participants != nil {
		intake, metadata = c.participants(db)
	}
	if c.metadataEnabled != (metadata != nil) {
		return nil, nil, domain.ErrOwnerInboxParticipantMissing
	}
	return intake, metadata, nil
}

func inboxHash(value any) string {
	b, err := json.Marshal(value)
	if err != nil {
		panic(err)
	} // Only concrete, JSON-safe internal plan structs.
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// InboxTextHash pins the exact text supplied to ClaimBatch and BindAcceptAndAckAO.
func InboxTextHash(text string) string { return inboxHash(text) }

func (c *OwnerInboxCore) transaction(ctx context.Context, run func(*gen.Queries, gen.DBTX) error) error {
	// Public core entrypoints must never join an ambient provider projection.
	if ctx.Value(conversationProjectionTxKey{}) != nil || ctx.Value(ownerInboxTransactionKey{}) != nil {
		return domain.ErrOwnerInboxConflict
	}
	if err := c.store.writeMu.LockContext(ctx); err != nil {
		return err
	}
	defer c.store.writeMu.Unlock()
	return c.store.inTxDB(ctx, "owner inbox", run)
}

func inboxAction(ctx context.Context, db gen.DBTX, project domain.ProjectID, key string) (domain.InboxReceipt, string, bool, error) {
	var receipt domain.InboxReceipt
	var raw, fingerprint string
	err := db.QueryRowContext(ctx, `SELECT receipt_json,fingerprint FROM owner_inbox_actions WHERE project_id=? AND action_key=?`, project, key).Scan(&raw, &fingerprint)
	if errors.Is(err, sql.ErrNoRows) {
		return receipt, "", false, nil
	}
	if err != nil {
		return receipt, "", false, err
	}
	err = json.Unmarshal([]byte(raw), &receipt)
	return receipt, fingerprint, true, err
}

func saveInboxAction(ctx context.Context, db gen.DBTX, receipt domain.InboxReceipt, fingerprint string) error {
	return saveInboxActionKey(ctx, db, receipt.Key, receipt, fingerprint)
}

func saveInboxActionKey(ctx context.Context, db gen.DBTX, key string, receipt domain.InboxReceipt, fingerprint string) error {
	raw, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `INSERT INTO owner_inbox_actions(project_id,action_key,stage,fingerprint,receipt_json) VALUES(?,?,?,?,?)`, receipt.ProjectID, key, receipt.Stage, fingerprint, string(raw))
	return err
}

// GetByActionKey reads a committed result without acquiring authority or I/O.
func (c *OwnerInboxCore) GetByActionKey(ctx context.Context, project domain.ProjectID, key string) (domain.InboxReceipt, bool, error) {
	r, _, found, err := inboxAction(ctx, c.store.readDB, project, key)
	return r, found, err
}

func readInboxOwner(ctx context.Context, db gen.DBTX, project domain.ProjectID) (domain.InboxOwner, error) {
	var o domain.InboxOwner
	err := db.QueryRowContext(ctx, `SELECT project_id,session_id,epoch,conversation_id,branch_id,generation,policy_revision FROM project_orchestrator_owners WHERE project_id=?`, project).Scan(&o.ProjectID, &o.SessionID, &o.Epoch, &o.ConversationID, &o.BranchID, &o.Generation, &o.PolicyRevision)
	if errors.Is(err, sql.ErrNoRows) {
		return o, domain.ErrOwnerRoutingUnavailable
	}
	return o, err
}

func checkInboxOwner(ctx context.Context, db gen.DBTX, expected domain.InboxOwner) error {
	actual, err := readInboxOwner(ctx, db, expected.ProjectID)
	if err != nil {
		return err
	}
	if actual != expected {
		return domain.ErrOwnerInboxConflict
	}
	return checkInboxProjection(ctx, db, actual)
}

func checkInboxProjection(ctx context.Context, db gen.DBTX, o domain.InboxOwner) error {
	var valid bool
	err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sessions s JOIN conversations c ON c.current_session_id=s.id
 WHERE s.id=? AND s.project_id=? AND s.kind='orchestrator' AND s.is_terminated=0
 AND c.id=? AND c.project_id=s.project_id AND c.scope='project' AND c.active_branch_id=?
 AND (CASE WHEN s.session_mode='chat' THEN s.controller_generation ELSE s.runtime_launch_id END)=?)`, o.SessionID, o.ProjectID, o.ConversationID, o.BranchID, o.Generation).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return domain.ErrOwnerRoutingUnavailable
	}
	return nil
}

// CompareAndSwapOwner records an explicitly admitted owner fact; it is not a
// backfill or lifecycle bridge. Callers must first atomically align the legacy
// conversation/lease/runtime projection. No production caller is wired yet.
func (c *OwnerInboxCore) CompareAndSwapOwner(ctx context.Context, previous int64, next domain.InboxOwner) error {
	if next.ProjectID == "" || next.SessionID == "" || next.Generation == "" || next.Epoch != previous+1 || next.PolicyRevision < 1 {
		return domain.ErrOwnerInboxConflict
	}
	return c.transaction(ctx, func(_ *gen.Queries, db gen.DBTX) error {
		if err := checkInboxProjection(ctx, db, next); err != nil {
			return err
		}
		prior, err := readInboxOwner(ctx, db, next.ProjectID)
		if errors.Is(err, domain.ErrOwnerRoutingUnavailable) {
			if previous != 0 {
				return domain.ErrOwnerInboxConflict
			}
		} else if err != nil {
			return err
		} else if prior.Epoch != previous {
			return domain.ErrOwnerInboxConflict
		}
		res, err := db.ExecContext(ctx, `INSERT INTO project_orchestrator_owners(project_id,session_id,epoch,conversation_id,branch_id,generation,policy_revision)
 VALUES(?,?,?,?,?,?,?)
 ON CONFLICT(project_id) DO UPDATE SET session_id=excluded.session_id,epoch=excluded.epoch,conversation_id=excluded.conversation_id,branch_id=excluded.branch_id,generation=excluded.generation,policy_revision=excluded.policy_revision
 WHERE project_orchestrator_owners.epoch=?`, next.ProjectID, next.SessionID, next.Epoch, next.ConversationID, next.BranchID, next.Generation, next.PolicyRevision, previous)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err == nil && n != 1 {
			return domain.ErrOwnerInboxConflict
		}
		return err
	})
}

// AcceptReport persists the original source and atomic metadata obligation.
func (c *OwnerInboxCore) AcceptReport(ctx context.Context, intent domain.InboxReportIntent) (domain.InboxReceipt, error) {
	r := intent.Report
	r.Outputs = append([]domain.ReportOutput{}, r.Outputs...)
	if intent.Key == "" {
		return domain.InboxReceipt{}, domain.ErrInvalidReport
	}
	if err := r.Validate(); err != nil {
		return domain.InboxReceipt{}, err
	}
	key := "accept:" + inboxHash([]string{string(r.SessionID), intent.Key})
	fingerprint := inboxHash(struct {
		State         domain.ReportState
		Note, Message string
		Outputs       []domain.ReportOutput
	}{r.State, r.Note, r.Message, r.Outputs})
	var receipt domain.InboxReceipt
	err := c.transaction(ctx, func(q *gen.Queries, db gen.DBTX) error {
		prior, hash, found, err := inboxAction(ctx, db, r.ProjectID, key)
		if err != nil {
			return err
		}
		if found {
			if hash != fingerprint {
				return domain.ErrOwnerInboxConflict
			}
			receipt = prior
			return nil
		}
		var worker bool
		if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sessions WHERE id=? AND project_id=? AND kind='worker')`, r.SessionID, r.ProjectID).Scan(&worker); err != nil {
			return err
		}
		if !worker {
			return domain.ErrInvalidReport
		}
		if _, err := q.CreateReport(ctx, gen.CreateReportParams{ID: r.ID, SessionID: string(r.SessionID), ProjectID: string(r.ProjectID), State: string(r.State), Note: r.Note, Message: r.Message, CreatedAt: r.CreatedAt, AvailableAt: r.AvailableAt, SettlementDeadline: nullTime(r.SettlementDeadline), RepeatCount: r.RepeatCount}); err != nil {
			return err
		}
		for i, o := range r.Outputs {
			if err := q.CreateReportOutput(ctx, gen.CreateReportOutputParams{ReportID: r.ID, Position: int64(i), Kind: string(o.Kind), Reference: o.Reference, Label: o.Label}); err != nil {
				return err
			}
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO owner_inbox_sources(report_id,project_id,sender_id,caller_key,fingerprint,source_version) VALUES(?,?,?,?,?,1)`, r.ID, r.ProjectID, r.SessionID, intent.Key, fingerprint); err != nil {
			return err
		}
		receipt = domain.InboxReceipt{ProjectID: r.ProjectID, Key: key, Stage: "accept", ReportID: r.ID}
		owner, ownerErr := readInboxOwner(ctx, db, r.ProjectID)
		if ownerErr == nil {
			ownerErr = checkInboxProjection(ctx, db, owner)
		}
		if errors.Is(ownerErr, domain.ErrOwnerRoutingUnavailable) {
			receipt.SafeReason = "routing_unavailable"
		} else if ownerErr != nil {
			return ownerErr
		}
		{
			_, metadata, err := c.transactionParticipants(db)
			if err != nil {
				return err
			}
			if metadata != nil {
				refs, err := metadata.Enlist(context.WithValue(ctx, ownerInboxTransactionKey{}, true), domain.InboxMetadataPlan{ProjectID: r.ProjectID, Stage: "accept", Sources: []domain.InboxSourceVersion{{ReportID: r.ID, Version: 1}}})
				if err != nil {
					return err
				}
				receipt.MetadataRefs = refs
			}
		}
		return saveInboxAction(ctx, db, receipt, fingerprint)
	})
	if err != nil {
		return domain.InboxReceipt{}, err
	}
	return receipt, nil
}

// ClaimBatch fixes ordered membership after checking durable owner admission.
func (c *OwnerInboxCore) ClaimBatch(ctx context.Context, plan domain.InboxBatchPlan) (domain.InboxReceipt, error) {
	plan.Members = append([]domain.InboxSourceVersion(nil), plan.Members...)
	if plan.ProjectID == "" || plan.ProjectID != plan.Owner.ProjectID || plan.Key == "" || len(plan.Members) == 0 || plan.TextHash == "" {
		return domain.InboxReceipt{}, domain.ErrOwnerInboxConflict
	}
	// Owner epoch is admission, not identity. Plan membership/order/policy is fixed.
	fingerprint := inboxHash(struct {
		Members  []domain.InboxSourceVersion
		TextHash string
		Policy   int64
	}{plan.Members, plan.TextHash, plan.Owner.PolicyRevision})
	var receipt domain.InboxReceipt
	err := c.transaction(ctx, func(_ *gen.Queries, db gen.DBTX) error {
		prior, hash, found, err := inboxAction(ctx, db, plan.ProjectID, plan.Key)
		if err != nil {
			return err
		}
		if found {
			if hash != fingerprint || prior.Stage != "claim" {
				return domain.ErrOwnerInboxConflict
			}
			receipt = prior
			return nil
		}
		if err := checkInboxOwner(ctx, db, plan.Owner); err != nil {
			return err
		}
		var canonicalKey, canonicalHash string
		err = db.QueryRowContext(ctx, `SELECT b.batch_key,b.plan_hash FROM owner_inbox_members m JOIN owner_inbox_batches b ON b.project_id=m.project_id AND b.batch_key=m.batch_key WHERE m.report_id=? AND m.project_id=?`, plan.Members[0].ReportID, plan.ProjectID).Scan(&canonicalKey, &canonicalHash)
		if err == nil {
			if canonicalHash != fingerprint {
				return domain.ErrOwnerInboxConflict
			}
			prior, _, found, err := inboxAction(ctx, db, plan.ProjectID, canonicalKey)
			if err != nil {
				return err
			}
			if !found {
				return domain.ErrOwnerInboxConflict
			}
			receipt = prior
			return saveInboxActionKey(ctx, db, plan.Key, prior, fingerprint)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO owner_inbox_batches(project_id,batch_key,plan_hash,text_hash,policy_revision) VALUES(?,?,?,?,?)`, plan.ProjectID, plan.Key, fingerprint, plan.TextHash, plan.Owner.PolicyRevision); err != nil {
			return err
		}
		for i, member := range plan.Members {
			var valid bool
			if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM owner_inbox_sources s JOIN reports r ON r.id=s.report_id WHERE s.report_id=? AND s.project_id=? AND s.source_version=? AND r.delivery_state='pending')`, member.ReportID, plan.ProjectID, member.Version).Scan(&valid); err != nil {
				return err
			}
			if !valid {
				return domain.ErrOwnerInboxConflict
			}
			if _, err := db.ExecContext(ctx, `INSERT INTO owner_inbox_members(report_id,project_id,batch_key,position,source_version) VALUES(?,?,?,?,?)`, member.ReportID, plan.ProjectID, plan.Key, i, member.Version); err != nil {
				return fmt.Errorf("%w: fixed member: %w", domain.ErrOwnerInboxConflict, err)
			}
		}
		receipt = domain.InboxReceipt{ProjectID: plan.ProjectID, Key: plan.Key, Stage: "claim", BatchKey: plan.Key, OwnerEpoch: plan.Owner.Epoch}
		{
			_, metadata, err := c.transactionParticipants(db)
			if err != nil {
				return err
			}
			if metadata != nil {
				refs, err := metadata.Enlist(context.WithValue(ctx, ownerInboxTransactionKey{}, true), domain.InboxMetadataPlan{ProjectID: plan.ProjectID, Stage: "claim", Owner: plan.Owner, BatchKey: plan.Key, Sources: plan.Members, PolicyRevision: plan.Owner.PolicyRevision})
				if err != nil {
					return err
				}
				receipt.MetadataRefs = refs
			}
		}
		return saveInboxAction(ctx, db, receipt, fingerprint)
	})
	if err != nil {
		return domain.InboxReceipt{}, err
	}
	return receipt, nil
}

// BindAcceptAndAckAO commits canonical input, participants, mapping and AO ACK.
func (c *OwnerInboxCore) BindAcceptAndAckAO(ctx context.Context, plan domain.InboxInputPlan) (domain.InboxReceipt, error) {
	if plan.ProjectID == "" || plan.ProjectID != plan.Owner.ProjectID || plan.Key == "" || plan.BatchKey == "" || plan.At.IsZero() || plan.TurnID == "" || plan.MessageID == "" || plan.ClientMessageID == "" {
		return domain.InboxReceipt{}, domain.ErrOwnerInboxConflict
	}
	plan.Text = plan.SummaryText
	if plan.UserText != nil {
		plan.Text = domain.AppendWorkerReportContext(*plan.UserText, plan.SummaryText)
	}
	// Ignore generated message/turn IDs on retries; the fixed canonical intent wins.
	fingerprint := inboxHash(struct{ Batch, Text, ClientID, Existing string }{plan.BatchKey, plan.Text, plan.ClientMessageID, plan.ExistingTurnID})
	var receipt domain.InboxReceipt
	err := c.transaction(ctx, func(q *gen.Queries, db gen.DBTX) error {
		prior, hash, found, err := inboxAction(ctx, db, plan.ProjectID, plan.Key)
		if err != nil {
			return err
		}
		if found {
			if hash != fingerprint || prior.Stage != "bind" {
				return domain.ErrOwnerInboxConflict
			}
			receipt = prior
			return nil
		}
		// A competing direct/piggyback attempt must look up the canonical batch,
		// never enlist twice or allocate a second input with a fresh action key.
		var textHash string
		var policy int64
		if err := db.QueryRowContext(ctx, `SELECT text_hash,policy_revision FROM owner_inbox_batches WHERE project_id=? AND batch_key=?`, plan.ProjectID, plan.BatchKey).Scan(&textHash, &policy); err != nil {
			return err
		}
		if textHash != InboxTextHash(plan.SummaryText) {
			return domain.ErrOwnerInboxConflict
		}
		var acceptedKey string
		err = db.QueryRowContext(ctx, `SELECT action_key FROM owner_inbox_acceptances WHERE project_id=? AND batch_key=?`, plan.ProjectID, plan.BatchKey).Scan(&acceptedKey)
		if err == nil {
			prior, _, found, err := inboxAction(ctx, db, plan.ProjectID, acceptedKey)
			if err != nil {
				return err
			}
			if !found {
				return domain.ErrOwnerInboxConflict
			}
			receipt = prior
			return saveInboxActionKey(ctx, db, plan.Key, prior, fingerprint)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err := checkInboxOwner(ctx, db, plan.Owner); err != nil {
			return err
		}
		if policy != plan.Owner.PolicyRevision {
			return domain.ErrOwnerInboxConflict
		}
		intake, metadata, err := c.transactionParticipants(db)
		if err != nil {
			return err
		}
		if intake == nil {
			return domain.ErrOwnerInboxParticipantMissing
		}
		members, err := q.SelectInboxMembers(ctx, gen.SelectInboxMembersParams{ProjectID: string(plan.ProjectID), BatchKey: plan.BatchKey})
		if err != nil {
			return err
		}
		versions := make([]domain.InboxSourceVersion, 0, len(members))
		for _, m := range members {
			var version int64
			var state string
			if err := db.QueryRowContext(ctx, `SELECT s.source_version,r.delivery_state FROM owner_inbox_sources s JOIN reports r ON r.id=s.report_id WHERE s.report_id=? AND s.project_id=?`, m.ReportID, plan.ProjectID).Scan(&version, &state); err != nil {
				return err
			}
			if version != m.SourceVersion || state != "pending" {
				return domain.ErrOwnerInboxConflict
			}
			versions = append(versions, domain.InboxSourceVersion{ReportID: m.ReportID, Version: version})
		}
		if len(versions) == 0 {
			return domain.ErrOwnerInboxConflict
		}
		plan.Members = append([]domain.InboxSourceVersion(nil), versions...)
		if err := bindInboxInput(ctx, db, plan); err != nil {
			return err
		}
		sReceipt, err := intake.EvaluateAndBind(context.WithValue(ctx, ownerInboxTransactionKey{}, true), plan)
		if err != nil {
			return err
		}
		if sReceipt.InputID != plan.TurnID || sReceipt.ReceiptRef == "" {
			return domain.ErrOwnerInboxConflict
		}
		receipt = domain.InboxReceipt{ProjectID: plan.ProjectID, Key: plan.Key, Stage: "bind", BatchKey: plan.BatchKey, TurnID: plan.TurnID, OwnerEpoch: plan.Owner.Epoch, SafeReason: sReceipt.SafeReason}
		if metadata != nil {
			refs, err := metadata.Enlist(context.WithValue(ctx, ownerInboxTransactionKey{}, true), domain.InboxMetadataPlan{ProjectID: plan.ProjectID, Stage: "bind", Owner: plan.Owner, BatchKey: plan.BatchKey, Sources: versions, PolicyRevision: policy})
			if err != nil {
				return err
			}
			receipt.MetadataRefs = refs
		}
		if err := saveInboxAction(ctx, db, receipt, fingerprint); err != nil {
			return err
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO owner_inbox_acceptances(project_id,batch_key,turn_id,action_key,owner_epoch,intake_receipt_ref) VALUES(?,?,?,?,?,?)`, plan.ProjectID, plan.BatchKey, plan.TurnID, plan.Key, plan.Owner.Epoch, sReceipt.ReceiptRef); err != nil {
			return err
		}
		for _, m := range members {
			res, err := db.ExecContext(ctx, `UPDATE reports SET delivery_state='acknowledged',acknowledged_at=?,delivery_batch_id=?,claim_token=?,claimed_at=? WHERE id=? AND delivery_state='pending'`, plan.At, plan.BatchKey, "owner-inbox:"+plan.BatchKey, plan.At, m.ReportID)
			if err != nil {
				return err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return err
			}
			if n != 1 {
				return domain.ErrOwnerInboxConflict
			}
		}
		return nil
	})
	if err != nil {
		return domain.InboxReceipt{}, err
	}
	return receipt, nil
}

func bindInboxInput(ctx context.Context, db gen.DBTX, p domain.InboxInputPlan) error {
	if p.ExistingTurnID != "" {
		var valid bool
		err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM conversation_turns t JOIN conversation_messages m ON m.turn_id=t.id
 WHERE t.id=? AND t.id=? AND t.conversation_id=? AND t.branch_id=? AND t.handled_by_session_id=? AND t.controller_generation=? AND t.state IN ('queued','held')
 AND m.id=? AND m.conversation_id=t.conversation_id AND m.client_message_id=? AND m.text=?)`, p.ExistingTurnID, p.TurnID, p.Owner.ConversationID, p.Owner.BranchID, p.Owner.SessionID, p.Owner.Generation, p.MessageID, p.ClientMessageID, p.Text).Scan(&valid)
		if err != nil {
			return err
		}
		if !valid {
			return domain.ErrOwnerInboxConflict
		}
		return nil
	}
	var sequence int64
	if err := db.QueryRowContext(ctx, `UPDATE conversations SET latest_sequence=latest_sequence+1,updated_at=? WHERE id=? AND current_session_id=? RETURNING latest_sequence`, p.At, p.Owner.ConversationID, p.Owner.SessionID).Scan(&sequence); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO conversation_turns(id,conversation_id,handled_by_session_id,controller_generation,state,requested_at,branch_id,intake_sequence) VALUES(?,?,?,?,'queued',?,?,?)`, p.TurnID, p.Owner.ConversationID, p.Owner.SessionID, p.Owner.Generation, p.At, p.Owner.BranchID, sequence); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, `INSERT INTO conversation_messages(id,conversation_id,turn_id,sequence,role,origin,text,client_message_id,created_at,updated_at,branch_id) VALUES(?,?,?,?,'user','daemon',?,?,?,?,?)`, p.MessageID, p.Owner.ConversationID, p.TurnID, sequence, p.Text, p.ClientMessageID, p.At, p.At, p.Owner.BranchID)
	return err
}
