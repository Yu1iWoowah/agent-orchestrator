package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
)

type delegationReadRequest struct {
	Question    string   `json:"question"`
	Paths       []string `json:"paths"`
	Focus       string   `json:"focus,omitempty"`
	OutputLimit int      `json:"output_limit,omitempty"`
}

func newDelegationCommand(c *commandContext) *cobra.Command {
	root := &cobra.Command{Use: "delegation", Short: "Control fixed optional HCG delegation"}
	for _, op := range []string{"get", "set", "clear", "emergency-stop", "profiles", "register", "read"} {
		var session, enabled, profile, question, focus string
		var paths []string
		var limit int
		var jsonOut bool
		cmd := &cobra.Command{Use: op, Args: noArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			method, path := http.MethodGet, "hcg-delegation/profiles"
			var body any
			if op == "get" || op == "set" || op == "clear" || op == "read" {
				if strings.TrimSpace(session) == "" || strings.TrimSpace(session) != session || strings.ContainsAny(session, "/\\\r\n\x00") {
					return usageError{fmt.Errorf("--session requires one session ID")}
				}
				path = "sessions/" + url.PathEscape(session) + "/hcg-delegation"
			}
			switch op {
			case "set", "emergency-stop":
				if enabled != "true" && enabled != "false" {
					return usageError{fmt.Errorf("--enabled must be true or false")}
				}
				method, path = http.MethodPut, path+"/override"
				body = struct {
					Enabled bool `json:"enabled"`
				}{enabled == "true"}
				if op == "emergency-stop" {
					path = "hcg-delegation/emergency-stop"
				}
			case "clear":
				method, path = http.MethodDelete, path+"/override"
			case "register":
				if !json.Valid([]byte(profile)) {
					return usageError{fmt.Errorf("--profile-json requires approved fixed profile JSON")}
				}
				method = http.MethodPut
				body = json.RawMessage(profile)
			case "read":
				if strings.TrimSpace(question) == "" || len(paths) == 0 || limit < 0 || limit > 65536 {
					return usageError{fmt.Errorf("--question and explicit --path values are required; output limit must be 0..65536 bytes")}
				}
				method, path = http.MethodPost, path+"/read"
				body = delegationReadRequest{Question: question, Paths: paths, Focus: focus, OutputLimit: limit}
			}
			var result json.RawMessage
			if err := c.doJSON(cmd.Context(), method, path, body, &result); err != nil {
				if op == "read" && cmd.Context().Err() != nil {
					_ = writeJSON(cmd.OutOrStdout(), delegationFailureJSON(cmd.Context()))
				}
				return err
			}
			if err := writeJSON(cmd.OutOrStdout(), result); err != nil {
				return err
			}
			var state usageReportResponse
			if op == "read" && (json.Unmarshal(result, &state) != nil || (state.Status != "ok" && state.Status != "not_found")) {
				return fmt.Errorf("hcg delegation: %s", state.Reason)
			}
			return nil
		}}
		cmd.Flags().BoolVar(&jsonOut, "json", false, "Output safe delegation JSON")
		if op == "get" || op == "set" || op == "clear" || op == "read" {
			cmd.Flags().StringVar(&session, "session", "", "AO session ID")
		}
		if op == "set" || op == "emergency-stop" {
			cmd.Flags().StringVar(&enabled, "enabled", "", "Explicit true or false")
		}
		if op == "register" {
			cmd.Flags().StringVar(&profile, "profile-json", "", "Complete approved fixed profile JSON; no key material")
		}
		if op == "read" {
			cmd.Flags().StringVar(&question, "question", "", "Question for the explicit approved scope")
			cmd.Flags().StringArrayVar(&paths, "path", nil, "Explicit relative file (repeatable)")
			cmd.Flags().StringVar(&focus, "focus", "", "Optional bounded focus")
			cmd.Flags().IntVar(&limit, "output-limit", 0, "Validated JSON byte limit; 0 uses 16384")
		}
		root.AddCommand(cmd)
	}
	root.AddCommand(&cobra.Command{Use: "mcp", Hidden: true, Args: noArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return c.serveDelegationMCP(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout())
	}})
	return root
}
func delegationFailureJSON(ctx context.Context) json.RawMessage {
	reason := "daemon_unavailable"
	if ctx.Err() == context.Canceled {
		reason = "cancelled"
	}
	if ctx.Err() == context.DeadlineExceeded {
		reason = "deadline_exceeded"
	}
	raw, _ := json.Marshal(map[string]any{"schema": "hcg.delegate-read/1", "status": "unavailable", "reason": reason, "answer": "", "findings": []any{}, "evidence": []any{}, "omissions": []any{}})
	return raw
}
