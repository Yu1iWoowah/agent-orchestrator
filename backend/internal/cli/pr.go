package cli

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

var mergeHeadSHAPattern = regexp.MustCompile(`^[0-9a-fA-F]{40}([0-9a-fA-F]{24})?$`)

type mergePRRequest struct {
	PRURL           string `json:"prUrl"`
	ExpectedHeadSHA string `json:"expectedHeadSha"`
}

type mergePRResponse struct {
	OK       bool   `json:"ok"`
	PRNumber int    `json:"prNumber"`
	Method   string `json:"method"`
}

type resolveCommentsRequest struct {
	CommentIDs []string `json:"commentIds,omitempty"`
}

type resolveCommentsResponse struct {
	OK       bool `json:"ok"`
	Resolved int  `json:"resolved"`
}

func newPRCommand(ctx *commandContext) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pr",
		Short: "Manage pull requests",
	}
	cmd.AddCommand(newPRMergeCommand(ctx))
	cmd.AddCommand(newPRResolveCommentsCommand(ctx))
	return cmd
}

func newPRMergeCommand(ctx *commandContext) *cobra.Command {
	var prURL, expectedHead string
	cmd := &cobra.Command{
		Use:   "merge <pr-number>",
		Short: "Merge a pull request",
		Long: "Merge a pull request at the explicitly approved head. Pass --pr-url and " +
			"--expected-head with the full commit SHA you reviewed; a changed head is rejected.",
		Example: "  ao pr merge 42 --pr-url https://github.com/owner/repo/pull/42 --expected-head 0123456789abcdef0123456789abcdef01234567",
		Args:    usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			prNumber, err := normalizePRNumber(args[0])
			if err != nil {
				return err
			}
			prURL = strings.TrimSpace(prURL)
			host, owner, repo, urlNumber, err := cliParsePRURL(prURL)
			if err != nil || owner == "" || repo == "" || strconv.Itoa(urlNumber) != prNumber {
				return usageError{errors.New("--pr-url must be a full HTTPS PR/MR URL matching the PR number")}
			}
			prURL = cliPRURLFromParts(strings.ToLower(host), owner, repo, urlNumber)
			expectedHead = strings.TrimSpace(expectedHead)
			if !mergeHeadSHAPattern.MatchString(expectedHead) {
				return usageError{errors.New("--expected-head must be the full reviewed commit SHA (40 or 64 hexadecimal characters)")}
			}
			var res mergePRResponse
			if err := ctx.postJSON(cmd.Context(), "prs/"+url.PathEscape(prNumber)+"/merge", mergePRRequest{PRURL: prURL, ExpectedHeadSHA: expectedHead}, &res); err != nil {
				return err
			}
			if method := strings.TrimSpace(res.Method); method != "" {
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "merged PR #%d using %s\n", res.PRNumber, method)
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "merged PR #%d\n", res.PRNumber)
			return err
		},
	}
	cmd.Flags().StringVar(&prURL, "pr-url", "", "Full PR/MR URL matching the PR number (required)")
	cmd.Flags().StringVar(&expectedHead, "expected-head", "", "Full reviewed commit SHA; reject a changed head (required)")
	return cmd
}

func newPRResolveCommentsCommand(ctx *commandContext) *cobra.Command {
	return &cobra.Command{
		Use:   "resolve-comments <pr-number> [comment-id...]",
		Short: "Resolve review threads on a pull request (by comment or thread id)",
		Args:  usageArgs(cobra.MinimumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			prNumber, err := normalizePRNumber(args[0])
			if err != nil {
				return err
			}
			commentIDs := make([]string, 0, len(args)-1)
			for _, id := range args[1:] {
				id = strings.TrimSpace(id)
				if id == "" {
					return usageError{errors.New("comment id must not be blank")}
				}
				commentIDs = append(commentIDs, id)
			}
			var res resolveCommentsResponse
			if err := ctx.postJSON(
				cmd.Context(),
				"prs/"+url.PathEscape(prNumber)+"/resolve-comments",
				resolveCommentsRequest{CommentIDs: commentIDs},
				&res,
			); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "resolved %d review thread(s) on PR #%s\n", res.Resolved, prNumber)
			return err
		},
	}
}

func normalizePRNumber(raw string) (string, error) {
	raw = strings.TrimPrefix(strings.TrimSpace(raw), "#")
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return "", usageError{errors.New("PR number must be a positive integer")}
	}
	return strconv.Itoa(n), nil
}
