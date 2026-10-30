package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// These DTOs mirror the public wire without importing daemon controllers.
// Raw responses preserve provider evidence and future fields in --json output.
type sessionSettingsPatchRequest struct {
	ExpectedRevision int64                     `json:"expectedRevision"`
	Settings         domain.AgentSettingsPatch `json:"settings"`
	ApplyMode        string                    `json:"applyMode"`
	StopTurn         bool                      `json:"stopTurn,omitempty"`
}

type sessionSettingsOperation struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionId"`
	Phase     string `json:"phase"`
	ErrorCode string `json:"errorCode"`
	RequestID string `json:"requestId"`
}

func sessionSettingsPath(id string) string {
	return "sessions/" + url.PathEscape(id) + "/agent-settings"
}

func newSessionSettingsCommand(c *commandContext) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{Use: "settings <id>", Short: "Read committed, pending and provider-reported agent settings", Args: oneSessionIDArg}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output the full settings response as JSON")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		id, err := normalizeSessionID(args[0])
		if err != nil {
			return err
		}
		var response json.RawMessage
		if err := c.getJSON(cmd.Context(), sessionSettingsPath(id), &response); err != nil {
			return err
		}
		if jsonOutput {
			return writeSettingsJSON(cmd, response)
		}
		var view struct {
			Revision  int64                `json:"revision"`
			Settings  domain.AgentSettings `json:"settings"`
			Pending   json.RawMessage      `json:"pending"`
			Effective json.RawMessage      `json:"effective"`
		}
		if err := json.Unmarshal(response, &view); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Session %s · revision %d\nCommitted: %s\nEffective: %s\n", id, view.Revision, settingsSelectionLabel(view.Settings), view.Effective); err != nil {
			return err
		}
		if len(view.Pending) > 0 && string(view.Pending) != "null" {
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Pending: %s\n", view.Pending)
		}
		return err
	}
	return cmd
}

func newSessionSetCommand(c *commandContext) *cobra.Command {
	var model, effort, fast string
	var revision int64
	var restart, stopTurn, wait, jsonOutput bool
	var waitTimeout time.Duration
	cmd := &cobra.Command{
		Use: "set <id>", Short: "Schedule revision-checked model, effort or fast settings",
		Long: "Schedule settings through the daemon. Model/effort default (or an empty value) resets the provider override. --restart consents to a TUI restart in the same conversation; --stop-turn additionally permits interruption. --wait timeout does not cancel an accepted operation. Account changes use ao account use separately.",
		Args: oneSessionIDArg,
	}
	f := cmd.Flags()
	f.StringVar(&model, "model", "", "Provider model ID or default")
	f.StringVar(&effort, "effort", "", "Advertised effort or default")
	f.StringVar(&fast, "fast", "", "Fast preference: on, off, or default")
	f.Int64Var(&revision, "if-revision", 0, "Expected settings revision (otherwise read before PATCH)")
	f.BoolVar(&restart, "restart", false, "Consent to a same-conversation TUI restart")
	f.BoolVar(&stopTurn, "stop-turn", false, "Permit interrupting the current TUI turn; requires --restart")
	f.BoolVar(&wait, "wait", false, "Wait for the accepted operation to finish")
	f.DurationVar(&waitTimeout, "wait-timeout", 2*time.Minute, "Maximum wait after acceptance; requires --wait")
	f.BoolVar(&jsonOutput, "json", false, "Output the full operation response as JSON")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		id, err := normalizeSessionID(args[0])
		if err != nil {
			return err
		}
		if !f.Changed("model") && !f.Changed("effort") && !f.Changed("fast") {
			return usageError{errors.New("provide --model, --effort, or --fast")}
		}
		if revision < 0 || stopTurn && !restart || waitTimeout <= 0 || f.Changed("wait-timeout") && !wait {
			return usageError{errors.New("revision must be non-negative; --stop-turn requires --restart; positive --wait-timeout requires --wait")}
		}
		patch := domain.AgentSettingsPatch{}
		if f.Changed("model") {
			value := domain.AgentModelSelection(settingsResetValue(model))
			patch.Model = &value
		}
		if f.Changed("effort") {
			value := domain.AgentEffortSelection(settingsResetValue(effort))
			patch.Effort = &value
		}
		if f.Changed("fast") {
			mode := domain.AgentFastMode(fast)
			if mode != domain.AgentFastOn && mode != domain.AgentFastOff && mode != domain.AgentFastDefault {
				return usageError{errors.New("--fast must be on, off, or default")}
			}
			patch.FastMode = &mode
		}
		if !f.Changed("if-revision") {
			var before struct {
				Revision *int64 `json:"revision"`
			}
			if err := c.getJSON(cmd.Context(), sessionSettingsPath(id), &before); err != nil {
				return err
			}
			if before.Revision == nil || *before.Revision < 0 {
				return errors.New("daemon did not provide an agent settings revision")
			}
			revision = *before.Revision
		}
		mode := "after-turn"
		if restart {
			mode = "restart"
		}
		body := sessionSettingsPatchRequest{ExpectedRevision: revision, Settings: patch, ApplyMode: mode, StopTurn: stopTurn}
		var response json.RawMessage
		if err := c.doJSONPathWithHeaders(cmd.Context(), http.MethodPatch, "/api/v1/"+sessionSettingsPath(id), body, &response, map[string]string{"Idempotency-Key": uuid.NewString()}); err != nil {
			return err
		}
		op, err := decodeSettingsOperation(response)
		if err != nil {
			return err
		}
		if err := settingsOperationFailure(op); err != nil {
			return err
		}
		if wait {
			response, err = c.waitSessionSettings(cmd.Context(), id, response, waitTimeout)
			if err != nil {
				return err
			}
		}
		return writeSettingsOperation(cmd, response, jsonOutput)
	}
	return cmd
}

func newSessionSettingsOperationCommand(c *commandContext) *cobra.Command {
	var sessionID string
	var jsonOutput bool
	cmd := &cobra.Command{Use: "settings-operation <operation-id>", Short: "Reconnect to a session settings operation", Args: oneSessionIDArg}
	cmd.Flags().StringVar(&sessionID, "session", "", "Target session ID (defaults to AO_SESSION_ID)")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output the full operation response as JSON")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if sessionID == "" {
			sessionID = os.Getenv("AO_SESSION_ID")
		}
		id, err := normalizeSessionID(sessionID)
		if err != nil {
			return usageError{errors.New("--session is required outside a session")}
		}
		opID, err := normalizeSessionID(args[0])
		if err != nil {
			return err
		}
		var response json.RawMessage
		if err := c.getJSON(cmd.Context(), sessionSettingsPath(id)+"/operations/"+url.PathEscape(opID), &response); err != nil {
			return err
		}
		return writeSettingsOperation(cmd, response, jsonOutput)
	}
	return cmd
}

func (c *commandContext) waitSessionSettings(ctx context.Context, id string, response json.RawMessage, timeout time.Duration) (json.RawMessage, error) {
	op, err := decodeSettingsOperation(response)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := settingsOperationFailure(op); err != nil {
			return nil, err
		}
		if op.Phase == "applied" {
			return response, nil
		}
		var next json.RawMessage
		err := c.getJSON(ctx, sessionSettingsPath(id)+"/operations/"+url.PathEscape(op.ID), &next)
		if err == nil {
			response = next
			op, err = decodeSettingsOperation(next)
			if err != nil {
				return nil, err
			}
			if op.Phase != "pending" {
				continue
			}
		} else if ctx.Err() == nil && !errors.Is(err, errDaemonUnavailable) {
			return nil, fmt.Errorf("settings operation %s: %w", op.ID, err)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("settings wait timeout or interruption; accepted operation %s was not cancelled; reconnect with ao session settings-operation %s --session %s: %w", op.ID, op.ID, id, ctx.Err())
		case <-ticker.C:
		}
	}
}

func settingsOperationFailure(op sessionSettingsOperation) error {
	switch op.Phase {
	case "applied", "pending":
		return nil
	case "failed", "cancelled", "recovery_required":
		return errors.New(apiError{Message: fmt.Sprintf("settings operation %s: %s", op.ID, op.Phase), Code: op.ErrorCode, RequestID: op.RequestID}.String())
	default:
		return fmt.Errorf("daemon returned unknown settings operation phase %q", op.Phase)
	}
}

func decodeSettingsOperation(response json.RawMessage) (sessionSettingsOperation, error) {
	var wire struct {
		Operation sessionSettingsOperation `json:"operation"`
	}
	if err := json.Unmarshal(response, &wire); err != nil {
		return sessionSettingsOperation{}, err
	}
	if wire.Operation.ID == "" || wire.Operation.Phase == "" {
		return sessionSettingsOperation{}, errors.New("daemon did not provide a settings operation")
	}
	return wire.Operation, nil
}

func writeSettingsOperation(cmd *cobra.Command, response json.RawMessage, jsonOutput bool) error {
	op, err := decodeSettingsOperation(response)
	if err != nil {
		return err
	}
	if jsonOutput {
		return writeSettingsJSON(cmd, response)
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "Operation %s: %s\n", op.ID, op.Phase)
	return err
}

func writeSettingsJSON(cmd *cobra.Command, response json.RawMessage) error {
	encoder := json.NewEncoder(cmd.OutOrStdout())
	encoder.SetIndent("", "  ")
	return encoder.Encode(response)
}

func settingsResetValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "default" {
		return ""
	}
	return value
}

func settingsSelectionLabel(settings domain.AgentSettings) string {
	model, effort := string(settings.Model), string(settings.Effort)
	if model == "" {
		model = "provider default"
	}
	if effort == "" {
		effort = "provider default"
	}
	return fmt.Sprintf("model=%s effort=%s fast=%s", model, effort, settings.FastMode)
}
