package sessionmanager

import (
	"context"
	"sync"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

type orchestratorGateContextKey struct{}
type orchestratorGateModeKey struct{}

type orchestratorGateMode uint8

const (
	orchestratorGateOrdinary orchestratorGateMode = iota
	orchestratorGateResume
	orchestratorGateNativeReplacement
	orchestratorGateSteer
)

// protected by the Manager's replacementGatesMu
type orchestratorProjectGate struct {
	mode     orchestratorGateMode
	released chan struct{}
}

type orchestratorGateReservation struct {
	mu         sync.Mutex
	gate       *orchestratorProjectGate
	references int
}
type orchestratorGateLease struct {
	manager     *Manager
	project     domain.ProjectID
	reservation *orchestratorGateReservation
	active      bool // protected by reservation.mu
}

func (lease *orchestratorGateLease) borrow() (*orchestratorGateLease, bool) {
	r := lease.reservation
	r.mu.Lock()
	defer r.mu.Unlock()
	if !lease.active {
		return nil, false
	}
	r.references++
	return &orchestratorGateLease{manager: lease.manager, project: lease.project, reservation: r, active: true}, true
}

func (lease *orchestratorGateLease) release() {
	r := lease.reservation
	r.mu.Lock()
	if !lease.active {
		r.mu.Unlock()
		return
	}
	lease.active = false
	r.references--
	last := r.references == 0
	r.mu.Unlock()
	if last {
		lease.manager.replacementGatesMu.Lock()
		close(r.gate.released)
		r.gate.released = nil
		lease.manager.replacementGatesMu.Unlock()
	}
}

type replacementAdmissionReader interface {
	GetActiveOrchestratorReplacement(context.Context, domain.ProjectID) (domain.OrchestratorReplacement, bool, error)
}

// acquireOrchestratorGate serializes all orchestrator lifecycle commands with
// replacement admission. A released context cannot bypass a later reservation.
func (m *Manager) acquireOrchestratorGate(ctx context.Context, project domain.ProjectID) (context.Context, func(), error) {
	if err := ctx.Err(); err != nil {
		return ctx, func() {}, err
	}
	if lease, ok := ctx.Value(orchestratorGateContextKey{}).(*orchestratorGateLease); ok && lease.manager == m && lease.project == project {
		if borrowed, ok := lease.borrow(); ok {
			return context.WithValue(ctx, orchestratorGateContextKey{}, borrowed), borrowed.release, nil
		}
	}
	mode, _ := ctx.Value(orchestratorGateModeKey{}).(orchestratorGateMode)
	m.replacementGatesMu.Lock()
	if m.replacementGates == nil {
		m.replacementGates = map[domain.ProjectID]*orchestratorProjectGate{}
	}
	gate := m.replacementGates[project]
	if gate == nil {
		gate = &orchestratorProjectGate{}
		m.replacementGates[project] = gate
	}
	for gate.released != nil {
		if mode == orchestratorGateSteer && (gate.mode == orchestratorGateResume || gate.mode == orchestratorGateNativeReplacement) {
			m.replacementGatesMu.Unlock()
			return ctx, func() {}, domain.ErrOrchestratorInputBusy
		}
		released := gate.released
		m.replacementGatesMu.Unlock()
		select {
		case <-released:
		case <-ctx.Done():
			return ctx, func() {}, ctx.Err()
		}
		m.replacementGatesMu.Lock()
	}
	if err := ctx.Err(); err != nil {
		m.replacementGatesMu.Unlock()
		return ctx, func() {}, err
	}
	gate.mode = mode
	gate.released = make(chan struct{})
	m.replacementGatesMu.Unlock()
	lease := &orchestratorGateLease{manager: m, project: project, reservation: &orchestratorGateReservation{gate: gate, references: 1}, active: true}
	return context.WithValue(ctx, orchestratorGateContextKey{}, lease), lease.release, nil
}

// WithOrchestratorProjectGate is shared by replacement reconciliation and the
// legacy session service; it does not serialize worker controller operations.
func (m *Manager) WithOrchestratorProjectGate(ctx context.Context, project domain.ProjectID, fn func(context.Context) error) error {
	locked, release, err := m.acquireOrchestratorGate(ctx, project)
	if err != nil {
		return err
	}
	defer release()
	return fn(locked)
}

// WithNativeReplacementProjectGate identifies the same gate's native
// replacement occupancy for public steer-or-send admission.
func (m *Manager) WithNativeReplacementProjectGate(ctx context.Context, project domain.ProjectID, fn func(context.Context) error) error {
	return m.WithOrchestratorProjectGate(context.WithValue(ctx, orchestratorGateModeKey{}, orchestratorGateNativeReplacement), project, fn)
}

func (m *Manager) beginReplacementSessionAction(ctx context.Context, id domain.SessionID) (context.Context, func(), error) {
	rec, found, err := m.store.GetSession(ctx, id)
	if err != nil || !found {
		return ctx, func() {}, err
	}
	return m.beginOrchestratorAction(ctx, rec.ProjectID, rec.Kind, id)
}

func (m *Manager) beginOrchestratorAction(ctx context.Context, project domain.ProjectID, kind domain.SessionKind, id domain.SessionID) (context.Context, func(), error) {
	if kind != domain.KindOrchestrator || project == "" {
		return ctx, func() {}, nil
	}
	locked, release, err := m.acquireOrchestratorGate(ctx, project)
	if err != nil {
		return ctx, func() {}, err
	}
	if reader, ok := m.store.(replacementAdmissionReader); ok {
		_, found, err := reader.GetActiveOrchestratorReplacement(locked, project)
		if err != nil || found {
			release()
			if err == nil {
				err = domain.ErrReplacementInProgress
			}
			return ctx, func() {}, err
		}
	}
	if reader, ok := m.store.(interface {
		GetRetiredOrchestratorReplacementBySource(context.Context, domain.SessionID) (domain.OrchestratorReplacement, bool, error)
	}); ok && id != "" {
		_, found, err := reader.GetRetiredOrchestratorReplacementBySource(locked, id)
		if err != nil || found {
			release()
			if err == nil {
				err = domain.ErrReplacementSourceRetired
			}
			return ctx, func() {}, err
		}
	}
	return locked, release, nil
}
