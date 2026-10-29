package cli

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

const appCapabilityHeader = "X-AO-App-Diagnostics-Capability"

var appIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

func newAppCommand(ctx *commandContext) *cobra.Command {
	var sessionID, grantID string
	var jsonOutput bool
	app := &cobra.Command{Use: "app", Short: "Read consented local desktop diagnostics", Args: noArgs}
	app.PersistentFlags().StringVar(&sessionID, "session", "", "authorized orchestrator session (defaults to AO_SESSION_ID)")
	app.PersistentFlags().StringVar(&grantID, "grant", "", "opaque grant id (required when multiple grants match)")
	app.PersistentFlags().BoolVar(&jsonOutput, "json", false, "print structured JSON")
	request := func(cmd *cobra.Command, method, path string, body any) (json.RawMessage, error) {
		id := sessionID
		if id == "" {
			id = os.Getenv("AO_SESSION_ID")
		}
		if !appIDPattern.MatchString(id) {
			return nil, usageError{errors.New("--session is required outside an AO session")}
		}
		if grantID != "" {
			if _, err := uuid.Parse(grantID); err != nil {
				return nil, usageError{errors.New("invalid --grant")}
			}
		}
		credential, err := loadAppCredential(id, grantID)
		if err != nil {
			return nil, err
		}
		// A redirect must never forward the local capability to another target.
		local := *ctx
		client := *ctx.deps.HTTPClient
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		local.deps.HTTPClient = &client
		var out json.RawMessage
		err = local.doJSONPathWithHeadersAndTimeout(cmd.Context(), method, "/api/v1/desktop/diagnostics/"+path, body, &out, map[string]string{appCapabilityHeader: credential.Token}, 8*time.Second)
		return out, err
	}
	read := func(name, path, short string) *cobra.Command {
		return &cobra.Command{Use: name, Short: short, Args: noArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			out, err := request(cmd, http.MethodGet, path, nil)
			if err != nil {
				return err
			}
			return writeJSON(cmd.OutOrStdout(), out)
		}}
	}
	app.AddCommand(read("status", "status", "Show the authorized app runtime"), read("windows", "windows", "List opaque authorized shell windows"))
	var window, project string
	var requireComplete bool
	snapshot := &cobra.Command{Use: "snapshot", Short: "Capture a read-only app snapshot without changing the UI", Args: noArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if (window != "" && !appIDPattern.MatchString(window)) || (project != "" && !appIDPattern.MatchString(project)) {
			return usageError{errors.New("invalid --window or --project")}
		}
		body := struct {
			WindowID  string `json:"windowId,omitempty"`
			ProjectID string `json:"projectId,omitempty"`
		}{window, project}
		out, err := request(cmd, http.MethodPost, "snapshots", body)
		if err != nil {
			return err
		}
		if err := writeJSON(cmd.OutOrStdout(), out); err != nil {
			return err
		}
		if requireComplete {
			var record struct {
				Snapshot struct {
					Completeness string `json:"completeness"`
				} `json:"snapshot"`
			}
			if json.Unmarshal(out, &record) != nil || record.Snapshot.Completeness != "complete" {
				return errors.New("APP_SNAPSHOT_PARTIAL: evidence returned; no retry performed")
			}
		}
		return nil
	}}
	snapshot.Flags().StringVar(&window, "window", "", "opaque shell window id (required if ambiguous)")
	snapshot.Flags().StringVar(&project, "project", "", "project observation filter within the grant scope")
	snapshot.Flags().BoolVar(&requireComplete, "require-complete", false, "exit 1 after printing a partial snapshot")
	snapshot.AddCommand(read("list", "snapshots", "List this grant's retained snapshots"))
	snapshot.AddCommand(&cobra.Command{Use: "get <snapshot-id>", Short: "Read a retained snapshot with a current grant", Args: exactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if !appIDPattern.MatchString(args[0]) {
			return usageError{errors.New("invalid snapshot id")}
		}
		out, err := request(cmd, http.MethodGet, "snapshots/"+args[0], nil)
		if err != nil {
			return err
		}
		return writeJSON(cmd.OutOrStdout(), out)
	}})
	app.AddCommand(snapshot)
	diagnostics := &cobra.Command{Use: "diagnostics", Short: "Read audit history or revoke this grant", Args: noArgs}
	diagnostics.AddCommand(read("audit", "audit", "Read bounded, content-free audit history"))
	diagnostics.AddCommand(&cobra.Command{Use: "revoke", Short: "Revoke this grant and clear its snapshots", Args: noArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if strings.TrimSpace(grantID) == "" {
			return usageError{errors.New("--grant is required")}
		}
		out, err := request(cmd, http.MethodDelete, "grants/"+grantID, nil)
		if err != nil {
			return err
		}
		return writeJSON(cmd.OutOrStdout(), out)
	}})
	app.AddCommand(diagnostics)
	return app
}
