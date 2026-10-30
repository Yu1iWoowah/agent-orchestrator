package cli

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// Account wire types intentionally omit profile paths and native credentials.
type accountInfo struct {
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
	ID        string `json:"id"`
	Phase     string `json:"phase"`
	ErrorCode string `json:"errorCode,omitempty"`
}
type accountTransitionResponse struct {
	Transition *accountTransition `json:"transition"`
}

func newAccountCommand(c *commandContext) *cobra.Command {
	cmd := &cobra.Command{Use: "account", Short: "Manage local provider accounts through the daemon"}
	cmd.AddCommand(newAccountListCommand(c), newAccountLoginCommand(c, false), newAccountLoginCommand(c, true), newAccountUseCommand(c), newAccountMutationCommand(c, false), newAccountMutationCommand(c, true))
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
	var provider string
	var refresh, jsonOut bool
	cmd := &cobra.Command{Use: "ls", Aliases: []string{"list"}, Short: "List account identity and sign-in state", Args: noArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if cmd.Flags().Changed("provider") && !validAccountProvider(provider) {
			return usageError{fmt.Errorf("--provider must be claude or codex")}
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
		w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
		if _, err := fmt.Fprintln(w, "ID\tPROVIDER\tIDENTITY\tSIGNED IN\tDEFAULT\tPROBLEM"); err != nil {
			return err
		}
		for _, a := range filtered {
			label := a.Email
			if a.Device {
				label = "This computer"
			}
			if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%t\t%t\t%s\n", a.ID, a.Provider, label, a.SignedIn, a.Default, a.Problem); err != nil {
				return err
			}
		}
		return w.Flush()
	}}
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
	f.StringVar(&project, "project", "", "Set a project provider or role default")
	f.StringVar(&role, "role", "", "Project role: worker, orchestrator, reviewer")
	f.StringVar(&reviewer, "reviewer", "", "Stable reviewer harness identity, never array position")
	f.StringVar(&session, "session", "", "Explicitly hand off this session")
	f.BoolVar(&stop, "stop-turn", false, "Interrupt the current turn before handoff (otherwise drain)")
	f.BoolVar(&wait, "wait", false, "Wait for the durable transition to finish")
	f.BoolVar(&jsonOut, "json", false, "Output selection or transition JSON")
	return cmd
}
func (c *commandContext) waitAccountTransition(ctx context.Context, session string, out *accountTransitionResponse) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	id := out.Transition.ID
	for {
		if out.Transition.Phase != "completed" && out.Transition.ErrorCode == "TARGET_STOP_UNCONFIRMED" {
			return fmt.Errorf("transition %s: TARGET_STOP_UNCONFIRMED; open the desktop and use Stop and retry", id)
		}
		switch out.Transition.Phase {
		case "completed":
			return nil
		case "failed", "cancelled", "recovery_required":
			return fmt.Errorf("transition %s: %s (%s)", id, out.Transition.Phase, out.Transition.ErrorCode)
		}
		if err := accountPoll(ctx); err != nil {
			return err
		}
		if err := c.getJSON(ctx, "sessions/"+url.PathEscape(session)+"/interface-transition", out); err != nil {
			return err
		}
		if out.Transition == nil || out.Transition.ID != id {
			return fmt.Errorf("transition %s is no longer the current transition; inspect session status", id)
		}
	}
}
func accountPoll(ctx context.Context) error {
	t := time.NewTimer(500 * time.Millisecond)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
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
