package cli

import (
	"bufio"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"
)

func newGuardianCommand(ctx *commandContext) *cobra.Command {
	var since, until, cursor string
	var details, jsonOutput bool
	var limit int
	cmd := &cobra.Command{Use: "guardian-observability <session-id>", Short: "Read local metadata-only guardian observations", Args: usageArgs(cobra.ExactArgs(1)), RunE: func(cmd *cobra.Command, args []string) error {
		if strings.TrimSpace(args[0]) == "" {
			return usageError{errors.New("session id is required")}
		}
		query := url.Values{}
		var start, end time.Time
		for key, raw := range map[string]string{"since": since, "until": until} {
			if raw == "" {
				continue
			}
			at, err := time.Parse(time.RFC3339Nano, raw)
			if err != nil {
				return usageError{errors.New("dates must be UTC RFC3339 timestamps")}
			}
			_, offset := at.Zone()
			if offset != 0 {
				return usageError{errors.New("dates must be UTC RFC3339 timestamps")}
			}
			query.Set(key, at.UTC().Format(time.RFC3339Nano))
			if key == "since" {
				start = at
			} else {
				end = at
			}
		}
		if !start.IsZero() && !end.IsZero() && !start.Before(end) {
			return usageError{errors.New("--since must precede --until")}
		}
		if cmd.Flags().Changed("limit") && (limit < 1 || limit > 500) || len(cursor) > 2048 || !details && (cmd.Flags().Changed("limit") || cursor != "") {
			return usageError{errors.New("--limit (1–500) and --cursor require --details")}
		}
		if details {
			query.Set("details", "true")
			if limit != 0 {
				query.Set("limit", fmt.Sprint(limit))
			}
		}
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		path := "sessions/" + url.PathEscape(args[0]) + "/guardian-observability"
		if len(query) > 0 {
			path += "?" + query.Encode()
		}
		var response guardianReportDTO
		if err := ctx.getJSON(cmd.Context(), path, &response); err != nil {
			return err
		}
		if jsonOutput {
			return writeJSON(cmd.OutOrStdout(), response)
		}
		out := cmd.OutOrStdout()
		if _, err := fmt.Fprintf(out, "Enabled: %t\nCoverage: %s\nReview events: %s (%s)\nRequests: %d\nReviews: %d\nUnmatched requests: %d\nExcluded conflicts: %d\nLedger reconciliation: %s\n", response.Enabled, response.Coverage, response.Capability.ReviewEvents, response.Capability.ReviewAvailability, response.Requests, response.Reviews, response.UnmatchedRequests, response.ExcludedConflicts, response.Reconciliation.State); err != nil {
			return err
		}
		if details {
			return writeJSON(out, response)
		}
		return writeJSON(out, struct {
			Totals  guardianTotalsDTO         `json:"totals"`
			Buckets []guardianDistributionDTO `json:"cache_buckets"`
		}{response.Totals, response.Buckets})
	}}
	cmd.Flags().StringVar(&since, "since", "", "Window start (UTC RFC3339)")
	cmd.Flags().StringVar(&until, "until", "", "Window end, exclusive (UTC RFC3339)")
	cmd.Flags().BoolVar(&details, "details", false, "Include bounded native metadata details")
	cmd.Flags().IntVar(&limit, "limit", 0, "Details page size (1–500; default 100)")
	cmd.Flags().StringVar(&cursor, "cursor", "", "Opaque details page token")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output safe metadata as JSON")
	cmd.AddCommand(newGuardianPurgeCommand(ctx))
	return cmd
}
func newGuardianPurgeCommand(ctx *commandContext) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{Use: "purge <session-id>", Short: "Delete this session's guardian observation metadata", Args: usageArgs(cobra.ExactArgs(1)), RunE: func(cmd *cobra.Command, args []string) error {
		if strings.TrimSpace(args[0]) == "" {
			return usageError{errors.New("session id is required")}
		}
		if !yes {
			input, ok := cmd.InOrStdin().(*os.File)
			if !ok || !term.IsTerminal(input.Fd()) {
				return usageError{errors.New("non-interactive purge requires --yes")}
			}
			if _, err := fmt.Fprint(cmd.OutOrStdout(), "Purge guardian observation metadata? Type yes to confirm: "); err != nil {
				return err
			}
			line, err := bufio.NewReader(input).ReadString('\n')
			if err != nil && line == "" {
				return err
			}
			if !strings.EqualFold(strings.TrimSpace(line), "yes") {
				_, err := fmt.Fprintln(cmd.OutOrStdout(), "aborted")
				return err
			}
		}
		var response struct {
			Purged bool `json:"purged"`
		}
		if err := ctx.deleteJSON(cmd.Context(), "sessions/"+url.PathEscape(args[0])+"/guardian-observability", &response); err != nil {
			return err
		}
		return writeJSON(cmd.OutOrStdout(), response)
	}}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Confirm deletion of observation metadata")
	return cmd
}
