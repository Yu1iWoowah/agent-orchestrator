package store

import (
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// LoadTranscriptSourceSnapshot never reconciles authentication or projects the
// active UI lineage. Every query uses the same SQLite read transaction.
func (s *Store) LoadTranscriptSourceSnapshot(ctx context.Context, id domain.SessionID) (out domain.TranscriptSourceSnapshot, err error) {
	tx, err := s.readDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer func() { _ = tx.Rollback() }()
	q := gen.New(tx)
	session, err := q.GetSession(ctx, id)
	if err != nil {
		return out, err
	}
	out.Session = rowToRecord(session)
	// Avoid volatile status/usage counters. Preserve all ownership/capability facts.
	proof := []any{out.Session.ControllerOwner(), out.Session.ProjectID, out.Session.Kind, out.Session.Metadata.NativeTranscriptPath, out.Session.Metadata.BrowserCapabilityVerifier, out.Session.Metadata.NativeCheckpointEvidence, out.Session.Metadata.ConversationCheckpointState, out.Session.Metadata.ConversationCheckpointGeneration, out.Session.Metadata.ConversationCheckpointNativeID, out.Session.Metadata.ConversationCheckpointTurnID, out.Session.Metadata.ConversationCheckpointUnsettled}
	conversations, err := q.SelectTranscriptConversations(ctx, &id)
	if err != nil {
		return out, err
	}
	out.ConversationCutoffs = map[string]int64{}
	for _, c := range conversations {
		out.ConversationCutoffs[c.ID] = c.LatestSequence
		proof = append(proof, struct {
			ID       string
			Sequence int64
			Active   string
		}{c.ID, c.LatestSequence, c.ActiveBranchID})
		if c.LatestSequence > out.CutoffSequence {
			out.CutoffSequence = c.LatestSequence
		}
		rows, e := q.SelectConversationBranches(ctx, c.ID)
		if e != nil {
			return out, e
		}
		proof = append(proof, rows)
		for _, r := range rows {
			out.Branches = append(out.Branches, conversationBranchListToDomain(r))
		}
		evidence, e := q.SelectConversationNativeThreadEvidence(ctx, gen.SelectConversationNativeThreadEvidenceParams{ConversationID: c.ID, SessionID: id})
		if e != nil {
			return out, e
		}
		slices.SortFunc(evidence, func(a, b gen.SelectConversationNativeThreadEvidenceRow) int {
			return cmp.Or(cmp.Compare(a.BranchID, b.BranchID), cmp.Compare(a.ProviderTurnID, b.ProviderTurnID), cmp.Compare(a.ProviderConversationID, b.ProviderConversationID))
		})
		proof = append(proof, evidence)
		for _, r := range evidence {
			out.NativeThreads = append(out.NativeThreads, domain.ConversationNativeThreadEvidence{BranchID: r.BranchID, ProviderTurnID: r.ProviderTurnID, ProviderConversationID: r.ProviderConversationID})
		}
	}
	turns, err := q.SelectTranscriptTurns(ctx, id)
	if err != nil {
		return out, err
	}
	proof = append(proof, turns)
	for _, r := range turns {
		out.Turns = append(out.Turns, turnToDomain(r))
	}
	messages, err := q.SelectTranscriptMessages(ctx, id)
	if err != nil {
		return out, err
	}
	proof = append(proof, messages)
	for _, r := range messages {
		out.Messages = append(out.Messages, messageToDomain(r))
	}
	activities, err := q.SelectTranscriptActivities(ctx, id)
	if err != nil {
		return out, err
	}
	proof = append(proof, activities)
	for _, r := range activities {
		out.Activities = append(out.Activities, activityToDomain(r))
	}
	terminal, err := q.SelectTranscriptTerminalTurns(ctx, string(id))
	if err != nil {
		return out, err
	}
	proof = append(proof, terminal)
	for _, r := range terminal {
		out.Terminal = append(out.Terminal, domain.TranscriptTerminalTurn{ID: r.ID, Harness: domain.AgentHarness(r.Harness), NativeID: r.NativeSessionID, Request: r.UserRequest, Response: r.AgentResponse, RequestedAt: r.RequestedAt, RespondedAt: transcriptTime(r.RespondedAt)})
	}
	native, err := q.SelectTranscriptNativeSessions(ctx, string(id))
	if err != nil {
		return out, err
	}
	proof = append(proof, native)
	for _, r := range native {
		out.NativeSessions = append(out.NativeSessions, domain.TranscriptNativeSession{Harness: domain.AgentHarness(r.Harness), NativeID: r.NativeSessionID, ConfigDir: r.ConfigDir, TranscriptPath: r.TranscriptPath, CreatedAt: r.CreatedAt})
	}
	transitions, err := q.SelectTranscriptInterfaceTransitions(ctx, id)
	if err != nil {
		return out, err
	}
	proof = append(proof, transitions)
	for _, row := range transitions {
		out.Transitions = append(out.Transitions, interfaceTransitionToDomain(row))
	}

	bindings, err := q.ListUsageBindingsForSession(ctx, id)
	if err != nil {
		return out, err
	}
	slices.SortFunc(bindings, func(a, b gen.UsageBinding) int { return cmp.Compare(a.ID, b.ID) })
	for _, r := range bindings {
		out.Bindings = append(out.Bindings, usageBindingFromGen(r))
		proof = append(proof, struct {
			ID      int64
			Harness domain.AgentHarness
			Root    string
		}{r.ID, r.Harness, r.NativeRootID})
		sources, e := q.ListUsageSourcesForBinding(ctx, r.ID)
		if e != nil {
			return out, e
		}
		slices.SortFunc(sources, func(a, b gen.UsageSource) int { return cmp.Compare(a.ID, b.ID) })
		for _, source := range sources {
			out.Sources = append(out.Sources, usageSourceFromGen(source))
			proof = append(proof, struct {
				ID                                 int64
				Kind                               domain.UsageSourceKind
				NativeID, Subagent, Path, Identity string
				Generation                         int64
			}{source.ID, source.Kind, source.NativeSessionID, source.SubagentID, source.ArtifactPath, source.FileIdentity, source.Generation})
		}
	}
	data, err := json.Marshal(proof)
	if err != nil {
		return out, fmt.Errorf("transcript snapshot proof: %w", err)
	}
	sum := sha256.Sum256(data)
	out.Fingerprint = hex.EncodeToString(sum[:])
	if err := tx.Commit(); err != nil {
		return domain.TranscriptSourceSnapshot{}, err
	}
	return out, nil
}

func transcriptTime(t sql.NullTime) *time.Time {
	if !t.Valid {
		return nil
	}
	return &t.Time
}
