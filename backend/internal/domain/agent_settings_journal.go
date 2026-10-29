package domain

// AgentSettingsApplyMode is the requested application boundary, not completion.
type AgentSettingsApplyMode string

// Settings application boundaries.
const (
	AgentSettingsAfterTurn AgentSettingsApplyMode = "after-turn"
	AgentSettingsRestart   AgentSettingsApplyMode = "restart"
)

// AgentSettingsCheckpoint records an action before crossing its provider boundary.
// A crash at an applying checkpoint must be treated as an uncertain outcome.
type AgentSettingsCheckpoint string

// Durable provider-action checkpoints.
const (
	AgentSettingsQueued         AgentSettingsCheckpoint = "queued"
	AgentSettingsApplyingModel  AgentSettingsCheckpoint = "applying_model"
	AgentSettingsApplyingEffort AgentSettingsCheckpoint = "applying_effort"
	AgentSettingsApplyingFast   AgentSettingsCheckpoint = "applying_fast"
	AgentSettingsVerifying      AgentSettingsCheckpoint = "verifying"
	AgentSettingsVerified       AgentSettingsCheckpoint = "verified"
	AgentSettingsRollingBack    AgentSettingsCheckpoint = "rolling_back"
	AgentSettingsRestored       AgentSettingsCheckpoint = "restored"
)

// CanAdvanceTo permits ordered application and an explicit, verified rollback.
func (s AgentSettingsCheckpoint) CanAdvanceTo(next AgentSettingsCheckpoint) bool {
	switch s {
	case AgentSettingsQueued, AgentSettingsApplyingModel, AgentSettingsApplyingEffort, AgentSettingsApplyingFast, AgentSettingsVerifying, AgentSettingsVerified, AgentSettingsRollingBack, AgentSettingsRestored:
	default:
		return false
	}
	if next == AgentSettingsRollingBack {
		return s != AgentSettingsQueued && s != AgentSettingsRollingBack && s != AgentSettingsRestored
	}
	if s == AgentSettingsRollingBack {
		return next == AgentSettingsRestored
	}
	order := map[AgentSettingsCheckpoint]int{AgentSettingsQueued: 0, AgentSettingsApplyingModel: 1, AgentSettingsApplyingEffort: 2, AgentSettingsApplyingFast: 3, AgentSettingsVerifying: 4}
	from, known := order[s]
	to, nextKnown := order[next]
	return known && nextKnown && to > from || s == AgentSettingsVerifying && next == AgentSettingsVerified
}

// AgentSettingsErrorCategory is a bounded audit classification, never raw stdout.
type AgentSettingsErrorCategory string

// Stable audit categories exclude raw provider errors.
const (
	AgentSettingsErrorConflict   AgentSettingsErrorCategory = "conflict"
	AgentSettingsErrorCancelled  AgentSettingsErrorCategory = "cancelled"
	AgentSettingsErrorProvider   AgentSettingsErrorCategory = "provider"
	AgentSettingsErrorStorage    AgentSettingsErrorCategory = "storage"
	AgentSettingsErrorValidation AgentSettingsErrorCategory = "validation"
	AgentSettingsErrorRecovery   AgentSettingsErrorCategory = "recovery_required"
)

// Valid rejects arbitrary provider text and credentials in audit categories.
func (c AgentSettingsErrorCategory) Valid() bool {
	switch c {
	case AgentSettingsErrorConflict, AgentSettingsErrorCancelled, AgentSettingsErrorProvider, AgentSettingsErrorStorage, AgentSettingsErrorValidation, AgentSettingsErrorRecovery:
		return true
	}
	return false
}
