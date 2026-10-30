package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

type replacementControlRetryKey struct{}

// Only positive terminal non-completion permits another read-only control turn.
// Unknown outcomes keep their original identity and lookup-only reconciliation.
func retryReplacementControl(ctx context.Context, q *gen.Queries, before domain.OrchestratorReplacement, after *domain.OrchestratorReplacement, at time.Time) (bool, error) {
	kind, clientID, turnID, hash := "", "", "", ""
	owner, ok := after.ExpectedSuccessorController()
	if !ok {
		return false, nil
	}
	if after.Phase == domain.ReplacementControllerReady && before.Bootstrap != nil && before.Bootstrap.SettledAt == nil {
		kind, clientID, turnID, hash = "bootstrap", before.BootstrapClientID, before.Bootstrap.TurnID, before.Bootstrap.PromptHash
	} else if p := before.ResumeEvidence; after.Phase == domain.ReplacementVerifyingResume && p != nil && p.Probe != nil && p.Probe.SettledAt == nil && p.VerifiedAt == nil {
		kind, clientID, turnID, hash = "probe", before.ResumeProbeClientID, p.Probe.TurnID, p.Probe.PromptHash
	}
	if turnID == "" {
		return false, nil
	}
	row, err := q.SelectConversationTurnByID(ctx, turnID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !row.State.Terminal() || row.State == domain.TurnStateCompleted {
		return false, nil
	}
	if err := verifyReplacementControlLedger(ctx, q, before, clientID, turnID, hash, owner.Owner, false, ""); err != nil {
		return false, err
	}
	receipt := domain.ReplacementControlAttempt{Kind: kind, ClientID: clientID, State: row.State, ArchivedAt: at.UTC()}
	if kind == "bootstrap" {
		b := *before.Bootstrap
		receipt.Bootstrap = &b
		after.Bootstrap = nil
		after.BootstrapClientID = fmt.Sprintf("replacement:%s:bootstrap:retry:%d", before.ID, len(before.ControlAttempts)+1)
	} else {
		p := *before.ResumeEvidence
		probe := *p.Probe
		receipt.Probe = &probe
		p.Probe = nil
		after.ResumeEvidence = &p
		after.ResumeProbeClientID = fmt.Sprintf("replacement:%s:probe:retry:%d", before.ID, len(before.ControlAttempts)+1)
	}
	after.ControlAttempts = append(slices.Clone(before.ControlAttempts), receipt)
	return true, nil
}

func preserveReplacementControlAttempts(ctx context.Context, q *gen.Queries, before, after domain.OrchestratorReplacement) error {
	if reflect.DeepEqual(before.ControlAttempts, after.ControlAttempts) {
		return nil
	}
	if ctx.Value(replacementControlRetryKey{}) != q || len(after.ControlAttempts) != len(before.ControlAttempts)+1 || len(before.ControlAttempts) > 0 && !reflect.DeepEqual(before.ControlAttempts, after.ControlAttempts[:len(before.ControlAttempts)]) {
		return domain.ErrReplacementStateInvalid
	}
	a := after.ControlAttempts[len(before.ControlAttempts)]
	if a.ClientID == "" || !a.State.Terminal() || a.State == domain.TurnStateCompleted || a.ArchivedAt.IsZero() {
		return domain.ErrReplacementStateInvalid
	}
	if a.Kind == "bootstrap" && a.ClientID == before.BootstrapClientID && reflect.DeepEqual(a.Bootstrap, before.Bootstrap) && a.Probe == nil && after.Bootstrap == nil && after.ResumeProbeClientID == before.ResumeProbeClientID {
		return nil
	}
	if a.Kind == "probe" && a.ClientID == before.ResumeProbeClientID && before.ResumeEvidence != nil && reflect.DeepEqual(a.Probe, before.ResumeEvidence.Probe) && a.Bootstrap == nil && after.ResumeEvidence != nil && after.ResumeEvidence.Probe == nil && after.BootstrapClientID == before.BootstrapClientID {
		return nil
	}
	return domain.ErrReplacementStateInvalid
}

func validateReplacementControlAttempts(op domain.OrchestratorReplacement) error {
	seen := map[string]bool{}
	for _, a := range op.ControlAttempts {
		if a.ClientID == "" || seen[a.ClientID] || !a.State.Terminal() || a.State == domain.TurnStateCompleted || a.ArchivedAt.IsZero() || a.ClientID == op.BootstrapClientID || a.ClientID == op.ResumeProbeClientID {
			return domain.ErrReplacementStateInvalid
		}
		seen[a.ClientID] = true
		switch a.Kind {
		case "bootstrap":
			if a.Bootstrap == nil || a.Probe != nil || a.Bootstrap.TurnID == "" || a.Bootstrap.SettledAt != nil || !replacementHash(a.Bootstrap.PromptHash) {
				return domain.ErrReplacementStateInvalid
			}
		case "probe":
			if a.Probe == nil || a.Bootstrap != nil || a.Probe.TurnID == "" || a.Probe.SettledAt != nil || !replacementHash(a.Probe.PromptHash) || !replacementUUID(a.Probe.Nonce) {
				return domain.ErrReplacementStateInvalid
			}
		default:
			return domain.ErrReplacementStateInvalid
		}
	}
	return nil
}
