package cli

import (
	"fmt"
	"net/url"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func newUsageCompositionCommand(c *commandContext) *cobra.Command {
	var session, review string
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "composition",
		Short: "Show each model request's input split by context category",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(session) == "" {
				return usageError{fmt.Errorf("--session is required")}
			}
			path := "usage/sessions/" + url.PathEscape(session) + "/composition"
			if review != "" {
				path += "?" + url.Values{"review": {review}}.Encode()
			}
			var out domain.UsageComposition
			if err := c.getJSON(cmd.Context(), path, &out); err != nil {
				return err
			}
			if jsonOut {
				return writeJSON(cmd.OutOrStdout(), out)
			}
			w := cmd.OutOrStdout()
			if out.NotCanonical {
				_, _ = fmt.Fprintln(w, "Reviewer: exact input from the rollout's token_count, not AO canonical usage.")
			}
			if out.Status != "ok" {
				_, _ = fmt.Fprintf(w, "UNKNOWN (%s)\n", out.Reason)
				return nil
			}
			tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
			header := []string{"#", "INPUT"}
			for _, cat := range out.Requests[0].Categories {
				header = append(header, strings.ToUpper(cat.Category))
			}
			_, _ = fmt.Fprintln(tw, strings.Join(header, "\t"))
			for _, req := range out.Requests {
				cells := []string{fmt.Sprint(req.Ordinal), compositionNumber(req.InputTokens)}
				if req.Unresolved > 0 {
					cells[1] += fmt.Sprintf(" (%d unresolved)", req.Unresolved)
				}
				for _, cat := range req.Categories {
					cells = append(cells, compositionNumber(cat.Tokens)+" / "+compositionNumber(cat.Bytes)+"B")
				}
				_, _ = fmt.Fprintln(tw, strings.Join(cells, "\t"))
			}
			return tw.Flush()
		},
	}
	cmd.Flags().StringVar(&session, "session", "", "AO session ID")
	cmd.Flags().StringVar(&review, "review", "", "Show this session's reviewer instead")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output JSON")
	return cmd
}

func compositionNumber(n *int64) string {
	if n == nil {
		return "-"
	}
	return fmt.Sprint(*n)
}
