package review

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/processscope"
	"github.com/aoagents/agent-orchestrator/backend/internal/procmem"
)

type reviewerStopStore interface {
	BeginReviewerStop(context.Context, domain.ReviewerStopReceipt) error
	UpdateReviewerStop(context.Context, domain.ReviewerStopReceipt, domain.ReviewerStopState, string, []domain.ReviewerStopProcess) error
	SettleReviewerStop(context.Context, domain.ReviewerStopReceipt, time.Time) error
	ListReviewerStopReceipts(context.Context) ([]domain.ReviewerStopReceipt, error)
	ListReviewerStopProcesses(context.Context, domain.ReviewerStopKey) ([]domain.ReviewerStopProcess, error)
	HasReviewerStop(context.Context, domain.ReviewerStopKey) (bool, error)
	HasUnsealedReviewerStop(context.Context, domain.SessionID) (bool, error)
}

type reviewerChatStopScope interface {
	ReviewChatStopScope(context.Context, string, string) (*domain.CompactionProcessScope, error)
	StopReviewChatGeneration(context.Context, string, string) error
}

func stopKey(review domain.Review) domain.ReviewerStopKey {
	return domain.ReviewerStopKey{ReviewID: review.ID, LaunchID: review.ReviewerLaunchID}
}

func (e *Engine) reviewerStopPending(ctx context.Context, review domain.Review) (bool, error) {
	store, ok := e.store.(reviewerStopStore)
	if !ok || review.ID == "" {
		return false, nil
	}
	return store.HasReviewerStop(ctx, stopKey(review))
}

func (e *Engine) checkReviewerStopFence(ctx context.Context, review domain.Review) error {
	pending, err := e.reviewerStopPending(ctx, review)
	if err != nil {
		return err
	}
	if pending {
		return fmt.Errorf("%w: reviewer stop is pending", ErrConflict)
	}
	return nil
}

func (e *Engine) checkUnsealedReviewerStop(ctx context.Context, sessionID domain.SessionID) error {
	store, ok := e.store.(reviewerStopStore)
	if !ok {
		return nil
	}
	pending, err := store.HasUnsealedReviewerStop(ctx, sessionID)
	if err != nil {
		return err
	}
	if pending {
		return fmt.Errorf("%w: %w", ErrConflict, domain.ErrReviewerStopPending)
	}
	return nil
}

func (l *agentLauncher) stopScope() (root, boot, namespace string) {
	if l.dataDir != "" {
		if abs, err := filepath.Abs(l.dataDir); err == nil {
			root, _ = filepath.EvalSymlinks(abs)
		}
	}
	if probe := l.forceStopHooks().comparisonScope; probe != nil {
		boot, namespace = probe()
	}
	return root, boot, namespace
}

func (l *agentLauncher) captureStopReceipt(ctx context.Context, review domain.Review) domain.ReviewerStopReceipt {
	root, boot, namespace := l.stopScope()
	receipt := domain.ReviewerStopReceipt{
		ReviewerStopKey: stopKey(review), SessionID: review.SessionID,
		HandleID: review.ReviewerHandleID, ControllerGeneration: review.ControllerGeneration,
		InterfaceMode: review.InterfaceMode, DataRoot: root, BootID: boot, PIDNamespace: namespace,
		State: domain.ReviewerStopCapturing,
	}
	if !hasReviewerRecoveryScope(receipt) {
		receipt.State, receipt.Reason = domain.ReviewerStopUnknown, "launch_scope_unavailable"
	}
	hooks := l.forceStopHooks()
	if review.InterfaceMode == domain.ReviewerInterfaceChat {
		chat, ok := l.chat.(reviewerChatStopScope)
		if ok && receipt.State != domain.ReviewerStopUnknown && receipt.ControllerGeneration != "" {
			scope, err := chat.ReviewChatStopScope(ctx, receipt.ReviewID, receipt.ControllerGeneration)
			if err == nil && scope != nil && scope.BootID == boot && scope.PIDNamespace == namespace {
				receipt.ChatScope = scope
				return receipt
			}
		}
		if receipt.Reason == "" {
			receipt.Reason = "chat_scope_unavailable"
		}
	} else {
		roots, err := l.reviewerRoots(ctx, ports.RuntimeHandle{ID: receipt.HandleID})
		if err == nil && len(roots) != 0 {
			identities, probeErr := hooks.identities(ctx)
			if probeErr == nil {
				processes, captureErr := capturedFromRoots(roots, identities, true)
				if captureErr == nil {
					receipt.Processes = durableProcesses(processes)
					return receipt
				}
			}
		}
		if receipt.Reason == "" {
			receipt.Reason = "reviewer_roots_unavailable"
		}
	}
	receipt.State = domain.ReviewerStopUnknown
	return receipt
}

func hasReviewerRecoveryScope(receipt domain.ReviewerStopReceipt) bool {
	return receipt.LaunchID != "" && receipt.DataRoot != "" && receipt.BootID != "" && receipt.PIDNamespace != ""
}

func durableProcesses(tree []capturedProcess) []domain.ReviewerStopProcess {
	out := make([]domain.ReviewerStopProcess, 0, len(tree))
	for _, p := range tree {
		out = append(out, domain.ReviewerStopProcess{PID: p.pid, Start: p.start, StartedAt: p.startedAt})
	}
	return out
}

func (e *Engine) rememberLocalReviewerLaunch(reviewID string, launch LaunchResult) {
	if launch.LaunchID == "" || launch.HandleID == "" {
		return
	}
	e.runs.mu.Lock()
	defer e.runs.mu.Unlock()
	if e.runs.localLaunches == nil {
		e.runs.localLaunches = make(map[string]LaunchResult)
	}
	e.runs.localLaunches[reviewID] = launch
}

func (e *Engine) beginDurableReviewerStop(ctx context.Context, store reviewerStopStore, launcher *agentLauncher, review domain.Review) (bool, error) {
	receipt, err := launcher.recordReviewerStop(ctx, store, review)
	if err != nil {
		return false, err
	}
	// A receipt can be created long after its reviewer launched. Writing it
	// cannot grant permission to stop a launch inherited from another daemon.
	e.runs.mu.Lock()
	launch, local := e.runs.localLaunches[receipt.ReviewID]
	if local && launch.LaunchID == receipt.LaunchID && launch.HandleID == receipt.HandleID {
		if e.runs.localStopKeys == nil {
			e.runs.localStopKeys = make(map[domain.ReviewerStopKey]bool)
		}
		e.runs.localStopKeys[receipt.ReviewerStopKey] = false
	}
	e.runs.mu.Unlock()
	// An unavailable initial probe with a known scope can be retried next
	// pass. It must not enter the same-daemon fallback for an absent scope.
	if receipt.State == domain.ReviewerStopUnknown && hasReviewerRecoveryScope(receipt) {
		return true, nil
	}
	return true, e.advanceReviewerStop(ctx, store, launcher, receipt)
}

func (l *agentLauncher) recordReviewerStop(ctx context.Context, store reviewerStopStore, review domain.Review) (domain.ReviewerStopReceipt, error) {
	receipt := l.captureStopReceipt(ctx, review)
	if err := store.BeginReviewerStop(ctx, receipt); err != nil {
		return receipt, err
	}
	return receipt, nil
}

func (e *Engine) resumeReviewerStops(ctx context.Context) error {
	store, ok := e.store.(reviewerStopStore)
	launcher, supported := e.launcher.(*agentLauncher)
	if !ok || !supported {
		return nil
	}
	receipts, err := store.ListReviewerStopReceipts(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, receipt := range receipts {
		unlock, ok := e.tryLockWorker(receipt.SessionID)
		if !ok {
			continue
		}
		err := e.advanceReviewerStop(ctx, store, launcher, receipt)
		unlock()
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func stopScopeMismatch(l *agentLauncher, r domain.ReviewerStopReceipt) string {
	root, boot, namespace := l.stopScope()
	switch {
	case root == "" || r.DataRoot == "":
		return "data_root_unavailable"
	case root != r.DataRoot:
		return "data_root_mismatch"
	case boot == "" || r.BootID == "":
		return "boot_scope_unavailable"
	case boot != r.BootID:
		return "boot_changed_origin_unverified"
	case namespace == "" || r.PIDNamespace == "" || namespace != r.PIDNamespace:
		return "pid_namespace_unverified"
	default:
		return ""
	}
}

func updateStopReason(ctx context.Context, store reviewerStopStore, receipt domain.ReviewerStopReceipt, reason string) error {
	if reason == receipt.Reason {
		return nil
	}
	return store.UpdateReviewerStop(ctx, receipt, receipt.State, reason, nil)
}

func (e *Engine) advanceReviewerStop(ctx context.Context, store reviewerStopStore, launcher *agentLauncher, receipt domain.ReviewerStopReceipt) error {
	e.runs.mu.Lock()
	confirmed, local := e.runs.localStopKeys[receipt.ReviewerStopKey]
	e.runs.mu.Unlock()
	if !confirmed {
		next, ended, err := e.observeReviewerStop(ctx, store, launcher, receipt, local)
		if err != nil || !ended {
			return err
		}
		receipt = next
		if local && receipt.State == domain.ReviewerStopUnknown {
			// Completion from the native fallback is valid only in this daemon.
			// Keep it for a SQLite retry if the handle/controller has now gone.
			e.runs.mu.Lock()
			e.runs.localStopKeys[receipt.ReviewerStopKey] = true
			e.runs.mu.Unlock()
		}
	}
	if err := store.SettleReviewerStop(ctx, receipt, e.clock()); err != nil {
		return err
	}
	e.runs.mu.Lock()
	delete(e.runs.localStopKeys, receipt.ReviewerStopKey)
	e.runs.mu.Unlock()
	return nil
}

func (e *Engine) observeReviewerStop(ctx context.Context, store reviewerStopStore, launcher *agentLauncher, receipt domain.ReviewerStopReceipt, local bool) (domain.ReviewerStopReceipt, bool, error) {
	hasRecoveryScope := hasReviewerRecoveryScope(receipt)
	if receipt.State != domain.ReviewerStopUnknown || hasRecoveryScope {
		if reason := stopScopeMismatch(launcher, receipt); reason != "" {
			return receipt, false, updateStopReason(ctx, store, receipt, reason)
		}
	} else if !local {
		return receipt, false, nil
	}
	reviews, err := e.store.ListReviewsBySession(ctx, receipt.SessionID)
	if err != nil {
		return receipt, false, err
	}
	var original domain.Review
	for _, review := range reviews {
		if stopKey(review) == receipt.ReviewerStopKey &&
			review.ReviewerHandleID == receipt.HandleID && review.ControllerGeneration == receipt.ControllerGeneration {
			original = review
			break
		}
	}
	current := original.ID != ""
	if receipt.State == domain.ReviewerStopUnknown {
		if !current || receipt.HandleID == "" {
			return receipt, false, nil
		}
		if !hasRecoveryScope {
			ended, err := launcher.stopReviewerWithoutRecoveryScope(ctx, store, receipt)
			return receipt, ended, err
		}
		captured := launcher.captureStopReceipt(ctx, original)
		if captured.State != domain.ReviewerStopCapturing || captured.DataRoot != receipt.DataRoot ||
			captured.BootID != receipt.BootID || captured.PIDNamespace != receipt.PIDNamespace {
			return receipt, false, nil
		}
		receipt.ChatScope = captured.ChatScope
		if err := store.UpdateReviewerStop(ctx, receipt, captured.State, "", captured.Processes); err != nil {
			return receipt, false, err
		}
		receipt.State, receipt.Reason = captured.State, ""
	}
	if receipt.InterfaceMode == domain.ReviewerInterfaceChat {
		ended, err := launcher.observeChatStop(ctx, receipt, local && current)
		return receipt, ended, err
	}
	return launcher.advanceTerminalStop(ctx, store, receipt, local, current)
}

func (l *agentLauncher) stopReviewerWithoutRecoveryScope(ctx context.Context, store reviewerStopStore, receipt domain.ReviewerStopReceipt) (bool, error) {
	if receipt.InterfaceMode == domain.ReviewerInterfaceChat {
		chat, ok := l.chat.(reviewerChatStopScope)
		if !ok || receipt.ControllerGeneration == "" {
			return false, nil
		}
		if err := chat.StopReviewChatGeneration(ctx, receipt.ReviewID, receipt.ControllerGeneration); err != nil {
			return false, err
		}
		return !l.chat.ReviewChatAlive(receipt.ReviewID), nil
	}
	tree, err := recordedStopProcesses(ctx, store, receipt.ReviewerStopKey)
	if err != nil {
		return false, err
	}
	hooks := l.forceStopHooks()
	seen := make(map[capturedProcess]struct{}, len(tree))
	for _, process := range tree {
		seen[process] = struct{}{}
	}
	hooks.record = func(ctx context.Context, members []capturedProcess) error {
		var added []capturedProcess
		for _, process := range members {
			if _, ok := seen[process]; !ok {
				added = append(added, process)
			}
		}
		if len(added) == 0 {
			return nil
		}
		if err := store.UpdateReviewerStop(ctx, receipt, receipt.State, receipt.Reason, durableProcesses(added)); err != nil {
			return fmt.Errorf("%w: %w", errRecordStopProcesses, err)
		}
		for _, process := range added {
			seen[process] = struct{}{}
		}
		return nil
	}
	return l.forceStopTerminal(ctx, receipt.HandleID, tree, hooks)
}

func (l *agentLauncher) observeChatStop(ctx context.Context, receipt domain.ReviewerStopReceipt, stop bool) (bool, error) {
	if receipt.ChatScope == nil || receipt.ChatScope.BootID != receipt.BootID || receipt.ChatScope.PIDNamespace != receipt.PIDNamespace {
		return false, nil
	}
	probe := l.forceStopHooks().chatEnded
	if probe == nil {
		probe = processscope.Ended
	}
	ended, reason := probe(ctx, receipt.ChatScope)
	if reason == "scope_boot_changed" {
		return false, nil
	}
	if ended || !stop {
		return ended, nil
	}
	chat, ok := l.chat.(reviewerChatStopScope)
	if !ok {
		return false, nil
	}
	if err := chat.StopReviewChatGeneration(ctx, receipt.ReviewID, receipt.ControllerGeneration); err != nil {
		return false, err
	}
	ended, reason = probe(ctx, receipt.ChatScope)
	return ended && reason != "scope_boot_changed", nil
}

func recordedStopProcesses(ctx context.Context, store reviewerStopStore, key domain.ReviewerStopKey) ([]capturedProcess, error) {
	processes, err := store.ListReviewerStopProcesses(ctx, key)
	if err != nil {
		return nil, err
	}
	tree := make([]capturedProcess, 0, len(processes))
	for _, p := range processes {
		tree = append(tree, capturedProcess{pid: p.PID, start: p.Start, startedAt: p.StartedAt})
	}
	return tree, nil
}

func (l *agentLauncher) advanceTerminalStop(ctx context.Context, store reviewerStopStore, receipt domain.ReviewerStopReceipt, local, current bool) (domain.ReviewerStopReceipt, bool, error) {
	tree, err := recordedStopProcesses(ctx, store, receipt.ReviewerStopKey)
	if err != nil {
		return receipt, false, err
	}
	if len(tree) == 0 {
		return receipt, false, updateStopReason(ctx, store, receipt, "lineage_unavailable")
	}
	hooks := l.forceStopHooks()
	handle := ports.RuntimeHandle{ID: receipt.HandleID}
	if receipt.State == domain.ReviewerStopCapturing {
		if !current {
			return receipt, false, updateStopReason(ctx, store, receipt, "lineage_unavailable")
		}
		tree, err = l.sealTerminalStop(ctx, store, receipt, handle, tree, hooks, local)
		if err != nil || len(tree) == 0 {
			return receipt, false, err
		}
		receipt.State = domain.ReviewerStopSealed
	}
	observe := hooks.identities
	hooks.identities = func(ctx context.Context) (map[int]procmem.Identity, error) {
		identities, err := observe(ctx)
		if err != nil {
			return nil, err
		}
		next, err := extendTree(tree, identities)
		if err != nil || len(next) != len(tree) || len(identities) == 0 {
			return nil, errors.Join(err, errors.New("sealed reviewer scope is inconclusive"))
		}
		return identities, nil
	}
	if local {
		remaining, err := killTreeSurvivors(ctx, tree, hooks)
		if err != nil {
			return receipt, false, err
		}
		if current {
			if err := l.runtime.Destroy(ctx, handle); err != nil {
				return receipt, false, err
			}
		}
		return receipt, len(remaining) == 0, nil
	}
	identities, err := hooks.identities(ctx)
	if err != nil {
		return receipt, false, err
	}
	for _, p := range tree {
		if current, ok := identities[p.pid]; ok && (!current.Known() || current.Start == p.start) {
			return receipt, false, nil
		}
	}
	return receipt, true, nil
}

func (l *agentLauncher) sealTerminalStop(ctx context.Context, store reviewerStopStore, receipt domain.ReviewerStopReceipt, handle ports.RuntimeHandle, tree []capturedProcess, hooks forceStopHooks, local bool) ([]capturedProcess, error) {
	if hooks.stopped == nil || hooks.freeze == nil {
		return nil, updateStopReason(ctx, store, receipt, "stop_probe_unavailable")
	}
	frozen := make(map[int]string)
	for range maxFreezeRounds {
		identities, err := hooks.identities(ctx)
		if err != nil {
			return nil, err
		}
		roots, err := l.reviewerRoots(ctx, handle)
		if err != nil {
			return nil, err
		}
		for _, root := range roots {
			current, live := identities[root.PID]
			original := false
			for _, process := range tree {
				if process.pid == root.PID && process.start == current.Start {
					original = true
					break
				}
			}
			if !live || !current.Known() || !original {
				return nil, updateStopReason(ctx, store, receipt, "lineage_unavailable")
			}
		}
		// All initial members, including the original runtime roots, must be
		// available to close a partial capture after a crash.
		if len(roots) == 0 {
			return nil, updateStopReason(ctx, store, receipt, "lineage_unavailable")
		}
		next, err := extendTree(tree, identities)
		if err != nil {
			return nil, err
		}
		if len(next) > len(tree) {
			if err := store.UpdateReviewerStop(ctx, receipt, receipt.State, receipt.Reason, durableProcesses(next[len(tree):])); err != nil {
				return nil, err
			}
			tree = next
		}
		stopped := true
		for _, p := range tree {
			if current, ok := identities[p.pid]; !ok || !current.Known() || current.Start != p.start {
				return nil, updateStopReason(ctx, store, receipt, "lineage_unavailable")
			}
			if hooks.stopped(p.pid, p.start) {
				continue
			}
			stopped = false
			if local && frozen[p.pid] != p.start {
				if err := hooks.freeze(p.pid); err != nil {
					return nil, errors.Join(err, updateStopReason(ctx, store, receipt, "freeze_unconfirmed"))
				}
				frozen[p.pid] = p.start
			}
		}
		if stopped {
			// The final snapshot is taken after every known process has stopped.
			// A late child visible here is persisted in the next iteration.
			final, err := hooks.identities(ctx)
			if err != nil {
				return nil, err
			}
			if len(final) == 0 {
				return nil, updateStopReason(ctx, store, receipt, "lineage_unavailable")
			}
			next, err := extendTree(tree, final)
			if err != nil {
				return nil, err
			}
			if len(next) == len(tree) {
				for _, process := range tree {
					if !hooks.stopped(process.pid, process.start) {
						return nil, updateStopReason(ctx, store, receipt, "freeze_unconfirmed")
					}
				}
				return tree, store.UpdateReviewerStop(ctx, receipt, domain.ReviewerStopSealed, "", nil)
			}
			if err := store.UpdateReviewerStop(ctx, receipt, receipt.State, receipt.Reason, durableProcesses(next[len(tree):])); err != nil {
				return nil, err
			}
			tree = next
			continue
		}
		if !local {
			break
		}
		timer := time.NewTimer(hooks.poll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, updateStopReason(ctx, store, receipt, "freeze_unconfirmed")
}
