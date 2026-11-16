package cli

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// Account wire types intentionally omit profile paths and native credentials.
type accountInfo struct {
	OrgID             string                     `json:"orgId,omitempty"`
	OrgName           string                     `json:"orgName,omitempty"`
	LoggedIn          bool                       `json:"loggedIn"`
	Limits            *domain.AgentAccountLimits `json:"limits,omitempty"`
	IdentityCheckedAt *time.Time                 `json:"identityCheckedAt,omitempty"`
	CreatedAt         *time.Time                 `json:"createdAt,omitempty"`
	ID                string                     `json:"id"`
	Provider          string                     `json:"provider"`
	Email             string                     `json:"email"`
	Plan              string                     `json:"plan"`
	Device            bool                       `json:"device"`
	Default           bool                       `json:"default"`
	SignedIn          bool                       `json:"signedIn"`
	Problem           string                     `json:"problem,omitempty"`
}
type accountList struct {
	Accounts []accountInfo `json:"accounts"`
}
type accountTransition struct {
	SourceAccountID string `json:"sourceAccountId,omitempty"`
	TargetAccountID string `json:"targetAccountId,omitempty"`
	ErrorDetail     string `json:"errorDetail,omitempty"`
	ID              string `json:"id"`
	Phase           string `json:"phase"`
	ErrorCode       string `json:"errorCode,omitempty"`
}
type accountTransitionResponse struct {
	Transition *accountTransition `json:"transition"`
}

func newAccountCommand(c *commandContext) *cobra.Command {
	cmd := &cobra.Command{Use: "account", Short: "Manage local provider accounts through the daemon"}
	cmd.AddCommand(newAccountListCommand(c), newAccountLoginCommand(c, false), newAccountLoginCommand(c, true), newAccountUseCommand(c), newAccountMutationCommand(c, false), newAccountMutationCommand(c, true), newAccountDefaultCommand(c), newAccountShowCommand(c), newAccountSwitchCommand(c))
	return cmd
}
func validAccountProvider(p string) bool { return p == "claude" || p == "codex" }
func accountIDArgs(cmd *cobra.Command, args []string) error {
	if len(args) != 1 || strings.TrimSpace(args[0]) == "" || strings.TrimSpace(args[0]) != args[0] || strings.ContainsAny(args[0], "/\\\x00\r\n") {
		return usageError{fmt.Errorf("%s requires one account ID", cmd.CommandPath())}
	}
	return nil
}
func newAccountListCommand(c *commandContext) *cobra.Command {
	var provider, agent string
	var refresh, jsonOut bool
	cmd := &cobra.Command{Use: "ls", Aliases: []string{"list"}, Short: "List account identity and sign-in state", Args: noArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if cmd.Flags().Changed("provider") && !validAccountProvider(provider) {
			return usageError{fmt.Errorf("--provider must be claude or codex")}
		}
		if cmd.Flags().Changed("agent") {
			selected, err := accountProvider(agent)
			if err != nil {
				return err
			}
			if provider != "" && provider != selected {
				return usageError{fmt.Errorf("--provider and --agent must select the same provider")}
			}
			provider = selected
		}
		path := "agents/accounts"
		if refresh {
			path += "?refresh=true"
		}
		var result accountList
		if err := c.getJSON(cmd.Context(), path, &result); err != nil {
			return err
		}
		filtered := make([]accountInfo, 0, len(result.Accounts))
		for _, a := range result.Accounts {
			if provider == "" || provider == a.Provider {
				filtered = append(filtered, a)
			}
		}
		result.Accounts = filtered
		if jsonOut {
			return writeJSON(cmd.OutOrStdout(), result)
		}
		return writeAccountList(cmd, result.Accounts)
	}}
	cmd.Flags().StringVar(&agent, "agent", "", "Filter claude-code or codex")
	cmd.Flags().StringVar(&provider, "provider", "", "Filter claude or codex")
	cmd.Flags().BoolVar(&refresh, "refresh", false, "Refresh native identity and limits")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output non-secret account JSON")
	return cmd
}
func newAccountUseCommand(c *commandContext) *cobra.Command {
	var global, stop, wait, jsonOut bool
	var project, session, role, reviewer string
	cmd := &cobra.Command{Use: "use <account-id>", Short: "Set a default or explicitly move a session", Args: accountIDArgs, RunE: func(cmd *cobra.Command, args []string) error {
		if (cmd.Flags().Changed("project") && strings.TrimSpace(project) == "") || (cmd.Flags().Changed("session") && strings.TrimSpace(session) == "") {
			return usageError{fmt.Errorf("--project and --session require a non-empty ID")}
		}
		scopes := 0
		for _, set := range []bool{global, project != "", session != ""} {
			if set {
				scopes++
			}
		}
		if scopes != 1 {
			return usageError{fmt.Errorf("choose exactly one of --default, --project, or --session")}
		}
		if role != "" && (project == "" || (role != "worker" && role != "orchestrator" && role != "reviewer")) {
			return usageError{fmt.Errorf("--role requires --project and worker, orchestrator, or reviewer")}
		}
		if project != "" && role == "" {
			return usageError{fmt.Errorf("--project requires --role worker, orchestrator, or reviewer; project provider defaults were removed")}
		}
		if (role == "reviewer" && reviewer == "") || (reviewer != "" && role != "reviewer") {
			return usageError{fmt.Errorf("--role reviewer requires --reviewer <harness>; --reviewer applies only to that role")}
		}
		if (stop || wait) && session == "" {
			return usageError{fmt.Errorf("--stop-turn and --wait require --session")}
		}
		id := url.PathEscape(args[0])
		var response any
		var transitionResult accountTransitionResponse
		if global {
			if err := c.postJSON(cmd.Context(), "agents/accounts/"+id+"/default", struct{}{}, nil); err != nil {
				return err
			}
			response = map[string]string{"accountId": args[0], "scope": "default"}
		}
		if project != "" {
			body := struct {
				Role     string `json:"role,omitempty"`
				Reviewer string `json:"reviewer,omitempty"`
			}{role, reviewer}
			if err := c.putJSON(cmd.Context(), "agents/accounts/"+id+"/projects/"+url.PathEscape(project), body, nil); err != nil {
				return err
			}
			response = map[string]string{"accountId": args[0], "scope": "project", "projectId": project, "role": role, "reviewer": reviewer}
		}
		if session != "" {
			var result accountTransitionResponse
			body := struct {
				AccountID string `json:"accountId"`
				StopTurn  bool   `json:"stopTurn"`
			}{args[0], stop}
			if err := c.putJSON(cmd.Context(), "agents/accounts/sessions/"+url.PathEscape(session), body, &result); err != nil {
				return err
			}
			if result.Transition == nil {
				return fmt.Errorf("daemon returned no durable transition")
			}
			if wait {
				if err := c.waitAccountTransition(cmd.Context(), session, &result); err != nil {
					return err
				}
			}
			response = result
			transitionResult = result
		}
		if jsonOut {
			return writeJSON(cmd.OutOrStdout(), response)
		}
		if session != "" {
			r := transitionResult
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "Transition %s: %s\n", r.Transition.ID, r.Transition.Phase)
			return err
		}
		_, err := fmt.Fprintln(cmd.OutOrStdout(), "Account selection saved; existing bindings are unchanged.")
		return err
	}}
	f := cmd.Flags()
	f.BoolVar(&global, "default", false, "Set this provider's global default for new work")
	f.StringVar(&project, "project", "", "Set a project role account (requires --role)")
	f.StringVar(&role, "role", "", "Project role: worker, orchestrator, reviewer")
	f.StringVar(&reviewer, "reviewer", "", "Stable reviewer harness identity, never array position")
	f.StringVar(&session, "session", "", "Explicitly hand off this session")
	f.BoolVar(&stop, "stop-turn", false, "Interrupt the current turn before handoff (otherwise drain)")
	f.BoolVar(&wait, "wait", false, "Wait for the durable transition to finish")
	f.BoolVar(&jsonOut, "json", false, "Output selection or transition JSON")
	return cmd
}
func (c *commandContext) waitAccountTransition(ctx context.Context, session string, out *accountTransitionResponse) error {
	if out.Transition == nil {
		return fmt.Errorf("daemon returned no durable transition")
	}
	current, err := c.awaitAccountSwitch(ctx, session, *out.Transition, 10*time.Minute)
	out.Transition = &current
	if err != nil {
		return err
	}
	if current.Phase != "completed" {
		return fmt.Errorf("transition %s: %s (%s)", current.ID, current.Phase, current.ErrorCode)
	}
	return nil
}
func newAccountMutationCommand(c *commandContext, remove bool) *cobra.Command {
	name, desc := "logout", "Sign out through the provider without deleting history"
	if remove {
		name, desc = "rm", "Retire a managed account; refuse live owners without stopping them"
	}
	return &cobra.Command{Use: name + " <account-id>", Short: desc, Args: accountIDArgs, RunE: func(cmd *cobra.Command, args []string) error {
		path := "agents/accounts/" + url.PathEscape(args[0])
		var err error
		if remove {
			err = c.deleteJSON(cmd.Context(), path, nil)
		} else {
			err = c.postJSON(cmd.Context(), path+"/logout", struct{}{}, nil)
		}
		if err != nil {
			var apiErr apiResponseError
			if errors.As(err, &apiErr) && apiErr.ErrorBody.Code == "AGENT_ACCOUNT_IN_USE" {
				var owners struct {
					Owners []struct {
						Kind        string `json:"kind"`
						SessionID   string `json:"sessionId"`
						ReviewID    string `json:"reviewId"`
						Blocking    bool   `json:"blocking"`
						SessionName string `json:"sessionName"`
						PRURL       string `json:"prUrl"`
					} `json:"owners"`
				}
				if e := c.getJSON(cmd.Context(), path+"/usage-owners", &owners); e == nil {
					for _, o := range owners.Owners {
						if !o.Blocking {
							continue
						}
						_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "In use: %s session=%s review=%s %s\n", o.Kind, o.SessionID, o.ReviewID, o.SessionName+" "+o.PRURL)
					}
				}
			}
			return err
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "Account %s completed.\n", name)
		return err
	}}
}

type sessionAgentAccounts struct {
	Claude string `json:"claude,omitempty"`
	Codex  string `json:"codex,omitempty"`
}

type switchSessionAccountRequest struct {
	AccountID string `json:"accountId"`
	StopTurn  bool   `json:"stopTurn,omitempty"`
}

type switchSessionAccountResponse struct {
	SessionID  string            `json:"sessionId"`
	Transition accountTransition `json:"transition"`
}

type sessionTransitionStatus struct {
	Transition *accountTransition `json:"transition,omitempty"`
}

var accountSwitchPoll = time.Second

func accountProvider(agent string) (string, error) {
	switch strings.TrimSpace(agent) {
	case "":
		return "", nil
	case "claude", "claude-code":
		return "claude", nil
	case "codex":
		return "codex", nil
	}
	return "", usageError{fmt.Errorf("--agent must be claude-code or codex, got %q", agent)}
}

func writeAccountList(cmd *cobra.Command, accounts []accountInfo) error {
	out := cmd.OutOrStdout()
	if len(accounts) == 0 {
		_, err := fmt.Fprintln(out, "No accounts.")
		return err
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tAGENT\tACCOUNT\tORG\tPLAN\tSTATE")
	for _, account := range accounts {
		state := "signed in"
		if !account.SignedIn {
			state = "signed out"
		}
		if account.Default {
			state += ", default"
		}
		if account.Problem != "" {
			state += " (" + account.Problem + ")"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", account.ID, accountAgentLabel(account.Provider), accountLabel(account), accountOrg(account, accounts), dash(account.Plan), state)
	}
	return tw.Flush()
}

func accountAgentLabel(provider string) string {
	if provider == "claude" {
		return "claude-code"
	}
	return provider
}

func accountLabel(account accountInfo) string {
	label := account.Email
	if label == "" {
		label = "(no email)"
	}
	if account.Device {
		label += " [this computer's login]"
	}
	return label
}

func accountOrg(account accountInfo, peers []accountInfo) string {
	label := account.OrgName
	for _, peer := range peers {
		if peer.ID == account.ID || account.OrgID == "" || peer.OrgID == account.OrgID || peer.OrgName != account.OrgName || !strings.EqualFold(peer.Email, account.Email) {
			continue
		}
		short := account.OrgID[:min(8, len(account.OrgID))]
		if label == "" {
			return short
		}
		return label + " (" + short + ")"
	}
	return dash(label)
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func newAccountDefaultCommand(ctx *commandContext) *cobra.Command {
	return &cobra.Command{
		Use:   "default <account-id>",
		Short: "Make an account the default for new sessions of its agent",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var res accountList
			if err := ctx.postJSON(cmd.Context(), "agents/accounts/"+url.PathEscape(args[0])+"/default", struct{}{}, &res); err != nil {
				return err
			}
			for _, account := range res.Accounts {
				if account.ID == args[0] {
					_, err := fmt.Fprintf(cmd.OutOrStdout(), "%s (%s) is now the default for new %s sessions\n", account.ID, accountLabel(account), accountAgentLabel(account.Provider))
					return err
				}
			}
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "%s is now the default\n", args[0])
			return err
		},
	}
}

func newAccountShowCommand(ctx *commandContext) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "show [session-id]",
		Short: "Show the account a session runs on (default: this session)",
		Args:  rangeArgs(0, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sessionID, err := accountSessionID(args)
			if err != nil {
				return err
			}
			if _, err := ctx.fetchScopedSession(cmd.Context(), sessionID, ""); err != nil {
				return err
			}
			var res sessionAgentAccounts
			if err := ctx.getJSON(cmd.Context(), "agents/accounts/sessions/"+url.PathEscape(sessionID), &res); err != nil {
				return err
			}
			if asJSON {
				return writeJSON(cmd.OutOrStdout(), res)
			}
			out := cmd.OutOrStdout()
			_, err = fmt.Fprintf(out, "claude-code: %s\ncodex: %s\n", dash(res.Claude), dash(res.Codex))
			return err
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Output JSON")
	return cmd
}

func accountSessionID(args []string) (string, error) {
	if len(args) > 0 && strings.TrimSpace(args[0]) != "" {
		return strings.TrimSpace(args[0]), nil
	}
	if id := strings.TrimSpace(os.Getenv("AO_SESSION_ID")); id != "" {
		return id, nil
	}
	return "", usageError{fmt.Errorf("pass a session id, or run inside an Orchestrator session")}
}

func newAccountSwitchCommand(ctx *commandContext) *cobra.Command {
	var stopTurn, wait bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "switch <session-id> <account-id>",
		Short: "Move a session to another account; the agent continues the same conversation there",
		Long: "Move a session to another account. The agent restarts in the same conversation on the new account.\n" +
			"A working agent finishes its turn first, unless --stop-turn stops it now.\n" +
			"An agent that moves its own session without --stop-turn moves after its turn ends, so do not pass --wait then.",
		Args: exactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			sessionID, accountID := args[0], args[1]
			var res switchSessionAccountResponse
			if err := ctx.putJSON(cmd.Context(), "agents/accounts/sessions/"+url.PathEscape(sessionID), switchSessionAccountRequest{AccountID: accountID, StopTurn: stopTurn}, &res); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if !wait {
				_, err := fmt.Fprintf(out, "moving %s to %s (transition %s, %s)\n", sessionID, accountID, res.Transition.ID, res.Transition.Phase)
				return err
			}
			done, err := ctx.awaitAccountSwitch(cmd.Context(), sessionID, res.Transition, timeout)
			if err != nil {
				return err
			}
			if done.Phase != "completed" {
				detail := done.ErrorDetail
				if detail == "" {
					detail = done.ErrorCode
				}
				return fmt.Errorf("move of %s to %s ended %s: %s", sessionID, accountID, done.Phase, dash(detail))
			}
			_, err = fmt.Fprintf(out, "moved %s to %s\n", sessionID, accountID)
			return err
		},
	}
	cmd.Flags().BoolVar(&stopTurn, "stop-turn", false, "Stop a turn in progress now instead of waiting for it to finish")
	cmd.Flags().BoolVar(&wait, "wait", false, "Wait until the move completes or fails")
	cmd.Flags().DurationVar(&timeout, "timeout", 3*time.Minute, "How long --wait waits")
	return cmd
}

func (c *commandContext) awaitAccountSwitch(ctx context.Context, sessionID string, started accountTransition, timeout time.Duration) (accountTransition, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	current := started
	for {
		if current.Phase != "completed" && current.ErrorCode == "TARGET_STOP_UNCONFIRMED" {
			return current, fmt.Errorf("transition %s: TARGET_STOP_UNCONFIRMED; open the desktop and use Stop and retry", current.ID)
		}
		switch current.Phase {
		case "completed", "failed", "cancelled", "recovery_required":
			return current, nil
		}
		select {
		case <-ctx.Done():
			return current, fmt.Errorf("move of %s still %s after %s; check it with `ao account show %s`", sessionID, current.Phase, timeout, sessionID)
		case <-time.After(accountSwitchPoll):
		}
		var status sessionTransitionStatus
		if err := c.getJSON(ctx, "sessions/"+url.PathEscape(sessionID)+"/interface-transition", &status); err != nil {
			return current, err
		}
		if status.Transition == nil {
			return current, fmt.Errorf("transition %s is no longer the current transition; inspect session status", started.ID)
		}
		if status.Transition.ID != started.ID {
			return current, fmt.Errorf("move of %s ended, but a later change (transition %s) replaced it before its outcome was read; check it with `ao account show %s`", sessionID, status.Transition.ID, sessionID)
		}
		if status.Transition != nil {
			current = *status.Transition
		}
	}
}
