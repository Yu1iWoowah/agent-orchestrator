package controllers

import (
	"encoding/json"
	"net/http"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
)

// UnmarshalJSON retains presence of legacy fields so even an empty legacy
// value cannot conflict with a typed selection for the same field.
func (in *SpawnSessionRequest) UnmarshalJSON(raw []byte) error {
	type wire SpawnSessionRequest
	var decoded wire
	fields := struct {
		*wire
		Model  json.RawMessage `json:"model"`
		Effort json.RawMessage `json:"effort"`
	}{wire: &decoded}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	decoded.legacyModelPresent = fields.Model != nil
	decoded.legacyEffortPresent = fields.Effort != nil
	if fields.Model != nil {
		if err := json.Unmarshal(fields.Model, &decoded.Model); err != nil {
			return err
		}
	}
	if fields.Effort != nil {
		if err := json.Unmarshal(fields.Effort, &decoded.Effort); err != nil {
			return err
		}
	}
	*in = SpawnSessionRequest(decoded)
	return nil
}

func (c *SessionsController) spawnCapabilities(w http.ResponseWriter, _ *http.Request) {
	envelope.WriteJSON(w, http.StatusOK, SpawnCapabilitiesResponse{AgentSettingsVersion: 1})
}
