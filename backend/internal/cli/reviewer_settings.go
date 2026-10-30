package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func reviewerSettingsOwnerArg(cmd *cobra.Command, args []string) error {
	if err := cobra.ExactArgs(1)(cmd, args); err != nil {
		return usageError{err}
	}
	if args[0] == "" || strings.TrimSpace(args[0]) != args[0] || strings.ContainsAny(args[0], "/\\\r\n") {
		return usageError{errors.New("provide a stable review ID from ao review ls --json, not a worker session ID")}
	}
	return nil
}

func reviewerSettingsPath(id string) string {
	return "reviews/" + url.PathEscape(id) + "/agent-settings"
}

func newReviewerSettingsCommand(c *commandContext) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{Use: "settings <reviewId>", Short: "Read a stable review owner's next-pass selection", Args: reviewerSettingsOwnerArg}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output preference, revision and effective unknown as JSON")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		var raw json.RawMessage
		if err := c.getJSON(cmd.Context(), reviewerSettingsPath(args[0]), &raw); err != nil {
			return err
		}
		if jsonOutput {
			return writeSettingsJSON(cmd, raw)
		}
		var state struct {
			Revision   int64                 `json:"revision"`
			Settings   *domain.AgentSettings `json:"settings"`
			ActivePass bool                  `json:"activePass"`
		}
		if err := json.Unmarshal(raw, &state); err != nil {
			return err
		}
		if state.Settings == nil {
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "%s revision=%d next-pass=unknown effective=unknown\n", args[0], state.Revision)
			return err
		}
		_, err := fmt.Fprintf(cmd.OutOrStdout(), "%s revision=%d next-pass: %s; active-pass=%t effective=unknown\n", args[0], state.Revision, settingsSelectionLabel(*state.Settings), state.ActivePass)
		return err
	}
	return cmd
}

func newReviewerSetCommand(c *commandContext) *cobra.Command {
	var model, effort, fast string
	var revision int64
	var jsonOutput bool
	cmd := &cobra.Command{Use: "set <reviewId>", Short: "Pin a revision-checked selection for the next review pass", Long: "Pin settings for a stable review ID. An active pass conflicts and is never interrupted. The next pass retains the reviewer's bound account and fixed permissions. This does not change worker settings or prove provider-effective values.", Args: reviewerSettingsOwnerArg}
	f := cmd.Flags()
	f.StringVar(&model, "model", "", "Reviewer model ID or default")
	f.StringVar(&effort, "effort", "", "Advertised reviewer effort or default")
	f.StringVar(&fast, "fast", "", "Fast preference: on, off, or default")
	f.Int64Var(&revision, "if-revision", 0, "Expected revision (otherwise read before PATCH)")
	f.BoolVar(&jsonOutput, "json", false, "Output the next-pass pin receipt as JSON")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if !f.Changed("model") && !f.Changed("effort") && !f.Changed("fast") || revision < 0 {
			return usageError{errors.New("provide --model, --effort, or --fast and a non-negative revision")}
		}
		patch := domain.AgentSettingsPatch{}
		if f.Changed("model") {
			patch.Model = new(domain.AgentModelSelection(settingsResetValue(model)))
		}
		if f.Changed("effort") {
			patch.Effort = new(domain.AgentEffortSelection(settingsResetValue(effort)))
		}
		if f.Changed("fast") {
			if fast != "on" && fast != "off" && fast != "default" {
				return usageError{errors.New("--fast must be on, off, or default")}
			}
			patch.FastMode = new(domain.AgentFastMode(fast))
		}
		if _, err := (domain.AgentSettings{FastMode: domain.AgentFastDefault}).Apply(patch); err != nil {
			return usageError{err}
		}
		if !f.Changed("if-revision") {
			var state struct {
				Revision *int64 `json:"revision"`
			}
			if err := c.getJSON(cmd.Context(), reviewerSettingsPath(args[0]), &state); err != nil {
				return err
			}
			if state.Revision == nil || *state.Revision < 0 {
				return errors.New("daemon did not provide a reviewer settings revision")
			}
			revision = *state.Revision
		}
		body := struct {
			ExpectedRevision int64                     `json:"expectedRevision"`
			Settings         domain.AgentSettingsPatch `json:"settings"`
		}{revision, patch}
		var raw json.RawMessage
		if err := c.doJSONPathWithHeaders(cmd.Context(), http.MethodPatch, "/api/v1/"+reviewerSettingsPath(args[0]), body, &raw, map[string]string{"Idempotency-Key": uuid.NewString()}); err != nil {
			return err
		}
		if jsonOutput {
			return writeSettingsJSON(cmd, raw)
		}
		var receipt struct {
			ID       string `json:"id"`
			Phase    string `json:"phase"`
			Revision int64  `json:"revision"`
		}
		if err := json.Unmarshal(raw, &receipt); err != nil {
			return err
		}
		if receipt.ID == "" || receipt.Phase != "pinned" {
			return errors.New("daemon did not confirm a reviewer next-pass pin")
		}
		_, err := fmt.Fprintf(cmd.OutOrStdout(), "Pinned %s revision=%d for the next review pass (operation %s); current pass unchanged\n", args[0], receipt.Revision, receipt.ID)
		return err
	}
	return cmd
}
