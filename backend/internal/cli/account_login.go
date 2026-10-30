package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/spf13/cobra"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/runfile"
)

type accountOperation struct {
	OperationID      string `json:"operationId"`
	AccountID        string `json:"accountId"`
	Provider         string `json:"provider"`
	Action           string `json:"action"`
	Phase            string `json:"phase"`
	Outcome          string `json:"outcome,omitempty"`
	ReasonCode       string `json:"reasonCode,omitempty"`
	ResultAccountID  string `json:"resultAccountId,omitempty"`
	TerminalHandleID string `json:"terminalHandleId,omitempty"`
}
type accountLoginStart struct {
	OperationID   string `json:"operationId"`
	Provider      string `json:"provider"`
	ShellTerminal struct {
		HandleID string `json:"handleId"`
	} `json:"shellTerminal"`
}

func newAccountLoginCommand(c *commandContext, again bool) *cobra.Command {
	var provider, operation string
	var detach, noAttach, jsonOut bool
	name, short := "add", "Add a managed subscription account in a daemon-owned login terminal"
	if again {
		name, short = "login", "Sign in again, or reconnect with --operation"
	}
	cmd := &cobra.Command{Use: name + map[bool]string{true: " [account-id]", false: ""}[again], Short: short, RunE: func(cmd *cobra.Command, args []string) error {
		if (!again && len(args) != 0) || (again && ((operation == "" && len(args) != 1) || (operation != "" && len(args) != 0))) {
			return usageError{fmt.Errorf("login requires an account ID or --operation <operation-id>; add takes --provider")}
		}
		if again && operation == "" {
			if err := accountIDArgs(cmd, args); err != nil {
				return err
			}
		}
		if again && cmd.Flags().Changed("operation") && strings.TrimSpace(operation) == "" {
			return usageError{fmt.Errorf("--operation requires a non-empty operation ID")}
		}
		if !again && !validAccountProvider(provider) {
			return usageError{fmt.Errorf("--provider must be claude or codex")}
		}
		if detach && noAttach {
			return usageError{fmt.Errorf("--detach and --no-attach cannot be combined")}
		}
		attach := !detach && !noAttach
		var input *os.File
		if attach {
			input, _ = cmd.InOrStdin().(*os.File)
			if input == nil || !term.IsTerminal(input.Fd()) {
				return usageError{fmt.Errorf("interactive login requires a terminal; use --detach to start or --no-attach to wait without PTY output")}
			}
			if jsonOut {
				return usageError{fmt.Errorf("--json requires --detach or --no-attach")}
			}
		}
		ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt)
		defer cancel()
		var handle string
		if operation != "" {
			var op accountOperation
			if err := c.getJSON(ctx, "agents/accounts/operations/"+url.PathEscape(operation), &op); err != nil {
				return err
			}
			if op.Action != "login" && op.Action != "login-again" {
				return usageError{fmt.Errorf("operation is not a login")}
			}
			if op.Phase == "complete" {
				return writeLoginOutcome(cmd, op, jsonOut)
			}
			handle = op.TerminalHandleID
		} else {
			path := "agents/accounts/login"
			var body any = map[string]string{"provider": provider}
			if again {
				path = "agents/accounts/" + url.PathEscape(args[0]) + "/login"
				body = struct{}{}
			}
			var start accountLoginStart
			// Finish obtaining the durable reference even if Ctrl+C arrives
			// during start; then cancel that exact operation with a fresh context.
			startCtx, startCancel := context.WithTimeout(context.WithoutCancel(ctx), commandTimeout)
			err := c.postJSON(startCtx, path, body, &start)
			startCancel()
			if err != nil {
				return err
			}
			operation, handle = start.OperationID, start.ShellTerminal.HandleID
		}
		if operation == "" {
			return fmt.Errorf("daemon returned no login operation ID")
		}
		if ctx.Err() != nil {
			return c.cancelAccountLogin(cmd, operation, jsonOut)
		}
		if detach {
			if jsonOut {
				return writeJSON(cmd.OutOrStdout(), map[string]string{"operationId": operation, "phase": "pending"})
			}
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "Login pending. Reconnect: ao account login --operation %s\n", operation)
			return err
		}
		if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "Login operation %s. Ctrl+C cancels; reconnect with ao account login --operation %s.\n", operation, operation); err != nil {
			return err
		}
		if attach && handle != "" {
			stop, err := c.attachAccountLogin(ctx, cancel, handle, input, cmd.OutOrStdout())
			if err != nil {
				return fmt.Errorf("login operation %s remains pending; reconnect: %w", operation, err)
			}
			defer stop()
		}
		for {
			var verified struct {
				Status string `json:"status"`
			}
			err := c.postJSON(ctx, "agents/accounts/login/"+url.PathEscape(operation)+"/verify", struct{}{}, &verified)
			if ctx.Err() != nil {
				return c.cancelAccountLogin(cmd, operation, jsonOut)
			}
			if err != nil {
				return err
			}
			var op accountOperation
			if err = c.getJSON(ctx, "agents/accounts/operations/"+url.PathEscape(operation), &op); err != nil {
				if ctx.Err() != nil {
					return c.cancelAccountLogin(cmd, operation, jsonOut)
				}
				return err
			}
			if op.Phase == "complete" {
				return writeLoginOutcome(cmd, op, jsonOut)
			}
			if err = accountPoll(ctx); err != nil {
				return c.cancelAccountLogin(cmd, operation, jsonOut)
			}
		}
	}}
	if !again {
		cmd.Flags().StringVar(&provider, "provider", "", "Subscription provider: claude or codex")
	} else {
		cmd.Flags().StringVar(&operation, "operation", "", "Reconnect to this daemon login operation; never creates a new profile")
	}
	cmd.Flags().BoolVar(&detach, "detach", false, "Return a pending operation reference without claiming completion")
	cmd.Flags().BoolVar(&noAttach, "no-attach", false, "Wait/verify without displaying or reading the login PTY")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output safe operation JSON; never a terminal transcript")
	return cmd
}
func writeLoginOutcome(cmd *cobra.Command, op accountOperation, jsonOut bool) error {
	op.TerminalHandleID = ""
	if op.Phase != "complete" {
		return fmt.Errorf("login %s is not confirmed", op.OperationID)
	}
	if op.Outcome != "complete" {
		return fmt.Errorf("login %s: %s (%s)", op.OperationID, op.Outcome, op.ReasonCode)
	}
	if jsonOut {
		return writeJSON(cmd.OutOrStdout(), op)
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "Login completed; account %s saved.\n", op.ResultAccountID)
	return err
}
func (c *commandContext) cancelAccountLogin(cmd *cobra.Command, id string, jsonOut bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	if err := c.postJSON(ctx, "agents/accounts/login/"+url.PathEscape(id)+"/cancel", struct{}{}, nil); err != nil {
		return fmt.Errorf("cancellation unconfirmed; reconnect with ao account login --operation %s: %w", id, err)
	}
	var op accountOperation
	if err := c.getJSON(ctx, "agents/accounts/operations/"+url.PathEscape(id), &op); err != nil {
		return err
	}
	if op.Phase == "complete" && op.Outcome == "complete" {
		return writeLoginOutcome(cmd, op, jsonOut)
	}
	if op.Phase != "complete" || op.Outcome != "cancelled" {
		return fmt.Errorf("login %s cancellation remains unconfirmed (%s)", id, op.ReasonCode)
	}
	_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "Login cancelled; daemon cleanup confirmed.")
	return context.Canceled
}

type accountTerminalFrame struct {
	Ch   string `json:"ch"`
	Type string `json:"type"`
	ID   string `json:"id,omitempty"`
	Data string `json:"data,omitempty"`
	Cols uint16 `json:"cols,omitempty"`
	Rows uint16 `json:"rows,omitempty"`
}

// Provider login prompts are live only. No replay/transcript file or credential
// file is read by this client. Known credential-shaped lines are suppressed;
// device authorization codes remain visible so the native login can complete.
var loginCredentialLine = regexp.MustCompile(`(?i)(access[_ -]?token|refresh[_ -]?token|id[_ -]?token|api[_ -]?key|authorization\s*:|bearer\s|sk-[A-Za-z0-9_-]{12}|eyJ[A-Za-z0-9_-]+\.)`)

// safeAccountLoginOutput classifies a completed line before the live PTY output.
func safeAccountLoginOutput(line string) string {
	// OSC stripping removes hyperlink URIs that may contain credentials.
	// Check raw controls as well as colour-split visible text.
	if loginCredentialLine.MatchString(line) || loginCredentialLine.MatchString(ansi.Strip(line)) {
		return "\r\n[credential output hidden]\r\n"
	}
	return line
}

type accountLoginOutput struct {
	pending    strings.Builder
	discarding bool
}

func (w *accountLoginOutput) write(out io.Writer, data []byte) {
	for len(data) > 0 {
		end := bytes.IndexByte(data, '\n')
		complete := end >= 0
		if complete {
			end++
		} else {
			end = len(data)
		}
		part := data[:end]
		data = data[end:]
		if !w.discarding {
			if w.pending.Len()+len(part) > 64*1024 {
				w.pending.Reset()
				w.discarding = true
				_, _ = io.WriteString(out, "\r\n[oversized login output hidden]\r\n")
			} else {
				w.pending.Write(part)
			}
		}
		if complete {
			if !w.discarding {
				_, _ = io.WriteString(out, safeAccountLoginOutput(w.pending.String()))
			}
			w.pending.Reset()
			w.discarding = false
		}
	}
}

func (c *commandContext) attachAccountLogin(ctx context.Context, cancel context.CancelFunc, id string, in *os.File, out io.Writer) (func(), error) {
	if id == "" {
		return nil, fmt.Errorf("login terminal unavailable; use --no-attach to verify")
	}
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	info, err := runfile.Read(cfg.RunFilePath)
	if err != nil {
		return nil, err
	}
	if info == nil || !c.deps.ProcessAlive(info.PID) {
		return nil, errDaemonNotRunning
	}
	conn, response, err := websocket.Dial(ctx, fmt.Sprintf("ws://%s:%d/mux", config.LoopbackHost, info.Port), &websocket.DialOptions{HTTPClient: &http.Client{Transport: c.deps.HTTPClient.Transport}})
	if response != nil && response.Body != nil {
		defer func() { _ = response.Body.Close() }()
	}
	if err != nil {
		return nil, fmt.Errorf("cannot attach login terminal")
	}
	conn.SetReadLimit(1 << 20)
	old, err := term.MakeRaw(in.Fd())
	if err != nil {
		_ = conn.CloseNow()
		return nil, err
	}
	attachCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	cleanup := func() { stop(); _ = conn.CloseNow(); <-done; _ = term.Restore(in.Fd(), old) }
	cols, rows, err := term.GetSize(in.Fd())
	if err != nil {
		cols, rows = 80, 24
	}
	if cols < 1 || cols > 1000 {
		cols = 80
	}
	if rows < 1 || rows > 1000 {
		rows = 24
	}
	if err = wsjson.Write(attachCtx, conn, accountTerminalFrame{Ch: "terminal", Type: "open", ID: id, Cols: uint16(cols), Rows: uint16(rows)}); err != nil {
		stop()
		_ = conn.CloseNow()
		_ = term.Restore(in.Fd(), old)
		return nil, fmt.Errorf("cannot open login terminal")
	}
	go func() {
		defer close(done)
		var output accountLoginOutput
		for {
			var f accountTerminalFrame
			if err := wsjson.Read(attachCtx, conn, &f); err != nil {
				return
			}
			if f.ID != id || f.Ch != "terminal" {
				continue
			}
			if f.Type == "data" {
				data, e := base64.StdEncoding.DecodeString(f.Data)
				if e != nil {
					continue
				}
				output.write(out, data)
			}
			if f.Type == "exited" || f.Type == "error" {
				return
			}
		}
	}()
	go func() {
		r := bufio.NewReader(in)
		for {
			ch, e := r.ReadByte()
			if e != nil {
				return
			}
			if ch == 3 {
				cancel()
				return
			}
			if e = wsjson.Write(attachCtx, conn, accountTerminalFrame{Ch: "terminal", Type: "data", ID: id, Data: base64.StdEncoding.EncodeToString([]byte{ch})}); e != nil {
				return
			}
		}
	}()
	return cleanup, nil
}
