package orchestratorreplacement

import (
	"context"
	"errors"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
)

// operatorStore is intentionally separate from admission/runtime dependencies.
// These data-only operations have no public transport until the 3D slice.
type operatorStore interface {
	DiagnoseOrchestratorReplacement(context.Context, string) (domain.ReplacementDiagnostic, bool, error)
	GetReplacementRepairSnapshot(context.Context, string) (domain.ReplacementRepairSnapshot, bool, error)
	RepairOrchestratorReplacement(context.Context, domain.ReplacementRepairRequest, time.Time) (domain.OrchestratorReplacement, bool, error)
}

func (s *Service) operatorStore() (operatorStore, error) {
	st, ok := s.deps.Store.(operatorStore)
	if !ok {
		return nil, apierr.NotImplemented("ORCHESTRATOR_REPLACEMENT_REPAIR_UNAVAILABLE", "Verified replacement repair is unavailable")
	}
	return st, nil
}

// Diagnose reads row identity/CAS metadata without decoding or mutating its facts.
func (s *Service) Diagnose(ctx context.Context, id string) (domain.ReplacementDiagnostic, error) {
	st, err := s.operatorStore()
	if err != nil {
		return domain.ReplacementDiagnostic{}, err
	}
	result, found, err := st.DiagnoseOrchestratorReplacement(ctx, id)
	if err == nil && !found {
		err = operationNotFound()
	}
	return result, err
}

// RepairSnapshot returns only store-sealed bytes, never an authenticated copy of
// the possibly malformed live row. Possession alone does not authorize a reset.
func (s *Service) RepairSnapshot(ctx context.Context, id string) (domain.ReplacementRepairSnapshot, error) {
	st, err := s.operatorStore()
	if err != nil {
		return domain.ReplacementRepairSnapshot{}, err
	}
	result, found, err := st.GetReplacementRepairSnapshot(ctx, id)
	if err == nil && !found {
		err = apierr.Conflict("ORCHESTRATOR_REPLACEMENT_REPAIR_UNVERIFIED", "No trusted repair snapshot exists; retain the fence", nil)
	}
	return result, err
}

// Repair restores known data under an explicit snapshot + raw revision/digest
// CAS. It neither advances the operation nor releases intake or the writer lease.
func (s *Service) Repair(ctx context.Context, in domain.ReplacementRepairRequest) (domain.OrchestratorReplacement, error) {
	var result domain.OrchestratorReplacement
	st, err := s.operatorStore()
	if err != nil {
		return result, err
	}
	trusted, err := s.RepairSnapshot(ctx, in.OperationID)
	if err != nil {
		return result, err
	}
	run := func(locked context.Context) error {
		diagnostic, err := s.Diagnose(locked, in.OperationID)
		if err != nil {
			return err
		}
		if !diagnostic.Corrupt {
			current, err := s.Get(locked, in.OperationID)
			if err != nil {
				return err
			}
			if !current.HasRepairReceipt(in) {
				return apierr.Conflict("ORCHESTRATOR_REPLACEMENT_REPAIR_CONFLICT", "Operation is healthy; no repair is needed", nil)
			}
		}
		var applied bool
		result, applied, err = st.RepairOrchestratorReplacement(locked, in, s.deps.Now().UTC())
		switch {
		case errors.Is(err, domain.ErrReplacementRepairConflict):
			return apierr.Conflict("ORCHESTRATOR_REPLACEMENT_REPAIR_CONFLICT", "Replacement row changed; diagnose again before repair", nil)
		case errors.Is(err, domain.ErrReplacementRepairUnverified):
			return apierr.Conflict("ORCHESTRATOR_REPLACEMENT_REPAIR_UNVERIFIED", "Snapshot is not the latest trusted record; retain the fence", nil)
		case errors.Is(err, domain.ErrReplacementRepairUnsafe):
			return apierr.Conflict("ORCHESTRATOR_REPLACEMENT_REPAIR_UNSAFE", "Repair cannot discard unknown native or managed facts; retain the fence", nil)
		case errors.Is(err, domain.ErrReplacementRepairCapacity):
			return apierr.Conflict("ORCHESTRATOR_REPLACEMENT_REPAIR_CAPACITY", "Repair evidence reached its storage limit; retain the fence for operator recovery", nil)
		case err != nil:
			return err
		case !applied && result.ID == "":
			return apierr.Conflict("ORCHESTRATOR_REPLACEMENT_REPAIR_CONFLICT", "No repair was applied", nil)
		default:
			return nil
		}
	}
	if s.deps.WithProjectGate != nil {
		err = s.deps.WithProjectGate(ctx, trusted.ProjectID, run)
	} else {
		unlock := s.lockProject(trusted.ProjectID)
		defer unlock()
		err = run(ctx)
	}
	return result, err
}
