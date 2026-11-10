package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// Keep CLI wire shapes independent of controllers and the reporting runner.
type usageReportResponse struct {
	Status string `json:"status"`
	Reason string `json:"reason"`
}

func newUsageCommand(c *commandContext) *cobra.Command {
	root := &cobra.Command{Use: "usage", Short: "Query usage through the daemon"}
	root.AddCommand(newUsageCompositionCommand(c))
	root.AddCommand(newDelegationCommand(c))
	integration := &cobra.Command{Use: "integration", Short: "Configure fixed optional usage reporting"}
	root.AddCommand(integration)
	for _, op := range []string{"get", "set", "clear", "emergency-stop", "register", "report"} {
		var session, enabled, executable, version, engine string
		var jsonOut bool
		cmd := &cobra.Command{Use: op, Args: noArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			path, method := "", http.MethodGet
			var body any
			switch op {
			case "get", "set", "clear", "report":
				if strings.TrimSpace(session) == "" || strings.TrimSpace(session) != session || strings.ContainsAny(session, "/\\\r\n\x00") {
					return usageError{fmt.Errorf("--session requires one session ID")}
				}
				path = "sessions/" + url.PathEscape(session) + "/hcg-reporting"
			}
			switch op {
			case "set", "emergency-stop":
				if enabled != "true" && enabled != "false" {
					return usageError{fmt.Errorf("--enabled must be true or false")}
				}
				body = struct {
					Enabled bool `json:"enabled"`
				}{enabled == "true"}
				method = http.MethodPut
				path += "/override"
				if op == "emergency-stop" {
					path = "hcg-reporting/emergency-stop"
				}
			case "clear":
				method, path = http.MethodDelete, path+"/override"
			case "register":
				if engine != "llm-usage-metrics" || !filepath.IsAbs(executable) || version != "0.14.3" {
					return usageError{fmt.Errorf("--engine llm-usage-metrics, --executable and --version 0.14.3 are required")}
				}
				method, path = http.MethodPut, "hcg-reporting/engine"
				body = struct {
					Engine          string `json:"engine"`
					Executable      string `json:"executable"`
					ExpectedVersion string `json:"expectedVersion"`
				}{engine, executable, version}
			case "report":
				if engine != "llm-usage-metrics" {
					return usageError{fmt.Errorf("--engine llm-usage-metrics is required")}
				}
				method, path = http.MethodPost, "usage/sessions/"+url.PathEscape(session)+"/report"
				body = struct {
					Engine string `json:"engine"`
				}{engine}
			}
			var result json.RawMessage
			if err := c.doJSON(cmd.Context(), method, path, body, &result); err != nil {
				return err
			}
			if err := writeJSON(cmd.OutOrStdout(), result); err != nil {
				return err
			}
			var state usageReportResponse
			if op == "report" && (json.Unmarshal(result, &state) != nil || state.Status != "ok") {
				return fmt.Errorf("usage report: %s", state.Reason)
			}
			return nil
		}}
		cmd.Flags().BoolVar(&jsonOut, "json", false, "Output safe reporting JSON")
		switch op {
		case "get", "set", "clear", "report":
			cmd.Flags().StringVar(&session, "session", "", "AO session ID")
		}
		if op == "set" || op == "emergency-stop" {
			cmd.Flags().StringVar(&enabled, "enabled", "", "Explicit true or false")
		}
		if op == "register" || op == "report" {
			cmd.Flags().StringVar(&engine, "engine", "", "llm-usage-metrics")
		}
		if op == "register" {
			cmd.Flags().StringVar(&executable, "executable", "", "Absolute installed engine executable")
			cmd.Flags().StringVar(&version, "version", "", "Fixed version 0.14.3")
		}
		if op == "report" {
			root.AddCommand(cmd)
		} else {
			integration.AddCommand(cmd)
		}
	}
	return root
}
