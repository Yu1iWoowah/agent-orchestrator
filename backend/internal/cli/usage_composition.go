package cli

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// Wire values use the domain's numeric projection, never controller or service
// objects. Every operation remains a single daemon HTTP call.
type compositionResponse domain.RCAComposition
type compositionPolicyResponse domain.RCAPolicyView

func newUsageCompositionCommand(c *commandContext) *cobra.Command {
	root := &cobra.Command{Use: "composition", Short: "Query recorded-content estimates and control RCA"}
	for _, op := range []string{"get", "set", "clear", "emergency-stop", "purge"} {
		var session, enabled string
		var analyze, yes, jsonOut bool
		var after, through, limit int64
		cmd := &cobra.Command{Use: op, Args: noArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			method, path := http.MethodGet, ""
			var body any
			if op != "emergency-stop" {
				if session == "" || strings.TrimSpace(session) != session || len(session) > 256 || session == "." || session == ".." || strings.ContainsAny(session, "/\\\r\n\x00") {
					return usageError{fmt.Errorf("--session requires one AO session ID")}
				}
				path = "usage/sessions/" + url.PathEscape(session) + "/composition"
			}
			var result any
			switch op {
			case "get":
				if after < 0 || limit < 1 || limit > 1000 || (!cmd.Flags().Changed("through-id") && after != 0) || (cmd.Flags().Changed("through-id") && (through < after || through < 0)) {
					return usageError{fmt.Errorf("invalid composition paging")}
				}
				q := url.Values{"afterId": {fmt.Sprint(after)}, "limit": {fmt.Sprint(limit)}}
				if cmd.Flags().Changed("through-id") {
					q.Set("throughId", fmt.Sprint(through))
				}
				if analyze {
					q.Set("analyze", "true")
				}
				path += "?" + q.Encode()
				result = &compositionResponse{}
			case "set", "emergency-stop":
				if enabled != "true" && enabled != "false" {
					return usageError{fmt.Errorf("--enabled must be true or false")}
				}
				method = http.MethodPut
				body = struct {
					Enabled bool `json:"enabled"`
				}{enabled == "true"}
				if op == "set" {
					path = "sessions/" + url.PathEscape(session) + "/rca/override"
					result = &compositionPolicyResponse{}
				} else {
					path = "rca/emergency-stop"
					result = &struct {
						Enabled bool `json:"enabled"`
					}{}
				}
			case "clear":
				method = http.MethodDelete
				path = "sessions/" + url.PathEscape(session) + "/rca/override"
				result = &compositionPolicyResponse{}
			case "purge":
				if !yes {
					return usageError{fmt.Errorf("purge requires --yes; canonical and native data are preserved")}
				}
				method = http.MethodDelete
				result = &struct {
					Purged bool `json:"purged"`
				}{}
			}
			if err := c.doJSON(cmd.Context(), method, path, body, result); err != nil {
				return err
			}
			return writeJSON(cmd.OutOrStdout(), result)
		}}
		cmd.Flags().BoolVar(&jsonOut, "json", false, "Output numeric JSON")
		if op != "emergency-stop" {
			cmd.Flags().StringVar(&session, "session", "", "Selected AO session")
		}
		if op == "set" || op == "emergency-stop" {
			cmd.Flags().StringVar(&enabled, "enabled", "", "Explicit true or false")
		}
		if op == "purge" {
			cmd.Flags().BoolVar(&yes, "yes", false, "Confirm numeric/scope purge")
		}
		if op == "get" {
			cmd.Flags().BoolVar(&analyze, "analyze", false, "Explicitly analyze the selected Codex worker source snapshot; Claude inline is launch-driven")
			cmd.Flags().Int64Var(&after, "after-id", 0, "Canonical paging cursor")
			cmd.Flags().Int64Var(&through, "through-id", 0, "Canonical insertion bound")
			cmd.Flags().Int64Var(&limit, "limit", 200, "Canonical page size (1..1000)")
		}
		root.AddCommand(cmd)
	}
	return root
}
