package cli

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"time"
	"unicode/utf8"

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
		phase := "pending"
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
			handle, phase = op.TerminalHandleID, op.Phase
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
		if phase == "pending" {
			var output io.Writer
			if attach {
				output = cmd.OutOrStdout()
			}
			exited, stop, err := c.observeAccountLogin(ctx, cancel, handle, input, output)
			if err != nil {
				if ctx.Err() != nil {
					return c.cancelAccountLogin(cmd, operation, jsonOut)
				}
				return fmt.Errorf("login operation %s remains pending; reconnect: %w", operation, err)
			}
			select {
			case err = <-exited:
			case <-ctx.Done():
				err = ctx.Err()
			}
			// Restore canonical mode before verification/cancellation messages.
			stop()
			if ctx.Err() != nil {
				return c.cancelAccountLogin(cmd, operation, jsonOut)
			}
			if err != nil {
				return fmt.Errorf("login operation %s has no confirmed terminal exit; reconnect: %w", operation, err)
			}
		}
		// Bound the whole post-exit verification phase as well as its retries.
		// RPC failures retain the operation so retrying cannot create another profile.
		verifyCtx, finishVerify := context.WithTimeout(ctx, commandTimeout)
		defer finishVerify()
		failed := func(err error) error {
			action := "resolve the reported problem before retrying"
			var response apiResponseError
			if errors.As(err, &response) && response.ErrorBody.Code == "AGENT_ACCOUNT_LEGACY_IDENTITY_CONFLICT" {
				// The daemon emits this only after completing legacy-conflict
				// cleanup; reconnecting that finished operation cannot retry it.
				return fmt.Errorf("login ended with a legacy identity conflict; remove the conflicting legacy Codex login in Settings > Accounts, then run ao account add --provider codex: %w", err)
			}
			if errors.Is(err, errDaemonNotRunning) {
				action = "start the daemon with ao start"
			} else if errors.As(err, &response) && response.ErrorBody.Code == "AGENT_ACCOUNT_STORAGE_UNAVAILABLE" {
				action = "check the account storage permissions in Settings"
			}
			return fmt.Errorf("login verification failed; %s; operation retained; reconnect with ao account login --operation %s: %w", action, operation, err)
		}
		const maxAttempts = 3
		for attempt := 1; ; attempt++ {
			var op accountOperation
			err := c.getJSON(verifyCtx, "agents/accounts/operations/"+url.PathEscape(operation), &op)
			if err == nil && op.Phase == "complete" {
				return writeLoginOutcome(cmd, op, jsonOut)
			}
			if err == nil {
				var verified struct {
					Status string `json:"status"`
				}
				err = c.postJSON(verifyCtx, "agents/accounts/login/"+url.PathEscape(operation)+"/verify", struct{}{}, &verified)
				if ctx.Err() != nil {
					return c.cancelAccountLogin(cmd, operation, jsonOut)
				}
				if err == nil && verified.Status == "waiting" {
					// The native process has exited unsigned. Reopening that
					// terminal cannot sign in; release this exact operation.
					cancelErr := c.cancelAccountLogin(cmd, operation, jsonOut)
					if cancelErr == nil { // Acceptance won; the account is saved.
						return nil
					}
					if !errors.Is(cancelErr, context.Canceled) {
						return fmt.Errorf("not signed in; %w", cancelErr)
					}
					retry := "ao account add --provider " + op.Provider
					if op.Action == "login-again" {
						retry = "ao account login " + op.AccountID
					}
					return fmt.Errorf("not signed in; login cancelled and cleanup confirmed; start a new attempt with %s", retry)
				}
				if err == nil {
					err = c.getJSON(verifyCtx, "agents/accounts/operations/"+url.PathEscape(operation), &op)
					if err == nil && op.Phase == "complete" {
						return writeLoginOutcome(cmd, op, jsonOut)
					}
					if err == nil {
						err = fmt.Errorf("login result remains unconfirmed")
					}
				}
			}
			if ctx.Err() != nil {
				return c.cancelAccountLogin(cmd, operation, jsonOut)
			}
			if !transientAccountLoginError(err) || attempt == maxAttempts || verifyCtx.Err() != nil {
				return failed(err)
			}
			delay := 500 * time.Millisecond * time.Duration(1<<(attempt-1))
			if _, writeErr := fmt.Fprintf(cmd.ErrOrStderr(), "Login verification attempt %d/%d failed: %v; retrying in %s.\n", attempt, maxAttempts, err, delay); writeErr != nil {
				return writeErr
			}
			timer := time.NewTimer(delay)
			select {
			case <-verifyCtx.Done():
				timer.Stop()
				if ctx.Err() != nil {
					return c.cancelAccountLogin(cmd, operation, jsonOut)
				}
				return failed(err)
			case <-timer.C:
			}
		}
	}}
	if !again {
		cmd.Flags().StringVar(&provider, "provider", "", "Subscription provider: claude or codex")
	} else {
		cmd.Flags().StringVar(&operation, "operation", "", "Reconnect to this daemon login operation; never creates a new profile")
	}
	cmd.Flags().BoolVar(&detach, "detach", false, "Return a pending operation reference without claiming completion")
	cmd.Flags().BoolVar(&noAttach, "no-attach", false, "Wait for terminal exit and verify without displaying PTY output")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output safe operation JSON; never a terminal transcript")
	return cmd
}
func transientAccountLoginError(err error) bool {
	var response apiResponseError
	if errors.As(err, &response) {
		if response.ErrorBody.Code == "AGENT_ACCOUNT_STORAGE_UNAVAILABLE" {
			return false
		}
		switch response.StatusCode {
		case http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return true
		default:
			return false
		}
	}
	return errors.Is(err, errDaemonUnavailable) && !errors.Is(err, errDaemonNotRunning)
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
	Role string `json:"role,omitempty"`
	Ch   string `json:"ch"`
	Type string `json:"type"`
	ID   string `json:"id,omitempty"`
	Data string `json:"data,omitempty"`
	Cols uint16 `json:"cols,omitempty"`
	Rows uint16 `json:"rows,omitempty"`
}

// Provider login prompts are live only. No replay/transcript file or credential
// file is read by this client. Known credential-shaped values are suppressed;
// device authorization codes remain visible so the native login can complete.
// Classify parameter-name families rather than provider-specific field names.
func accountCredentialKey(name string) (root, sensitive bool) {
	decoded, err := url.QueryUnescape(name)
	if err != nil || strings.ContainsRune(decoded, '%') {
		// An incomplete/ambiguous encoding is not proof of a harmless name.
		// Do not latch a partial escape, but hide its value at assignment.
		return false, true
	}
	name = strings.ToLower(decoded)
	separator := func(r rune) bool { return strings.ContainsRune("[] ./-_\"'\t\r\n", r) }
	fragments := strings.FieldsFunc(name, separator)
	trailingSeparator := name != "" && separator(rune(name[len(name)-1]))
	for i, fragment := range fragments {
		for _, family := range []string{"token", "secret", "password", "passwd", "credential", "apikey", "authorization", "bearer", "cookie", "session", "auth"} {
			if strings.Contains(fragment, family) {
				root = true
			}
		}
		if strings.HasSuffix(fragment, "key") {
			sensitive = true
			// A completed *key segment stays sensitive through later path
			// segments. A prefix like keyboard is not itself a *key name.
			root = root || i < len(fragments)-1 || trailingSeparator
		}
	}
	return root, root || sensitive
}

type accountLoginOutput struct {
	parser    *ansi.Parser
	key       string
	keyRoot   bool
	nameDepth int
	padding   int
	hiding    bool
}

func (w *accountLoginOutput) resetKey() {
	w.key, w.keyRoot, w.nameDepth, w.padding = "", false, 0, 0
}

func (w *accountLoginOutput) write(out io.Writer, data []byte) {
	if w.parser == nil {
		w.parser = ansi.NewParser()
		w.parser.SetDataSize(4096)
	}
	var visible strings.Builder
	hide := func() { w.hiding = true; visible.WriteString("[credential output hidden]") }
	printRune := func(r rune) {
		if w.hiding {
			return
		}
		switch {
		case r == '=' || r == ':':
			_, sensitive := accountCredentialKey(w.key)
			if w.keyRoot || sensitive {
				hide()
				return
			}
			w.resetKey()
		case r == ' ' || r == '\t':
			if strings.EqualFold(w.key, "bearer") {
				hide()
				return
			}
			if w.nameDepth == 0 {
				w.padding++
			}
		case r == '\'' || r == '"':
			if w.nameDepth == 0 {
				w.padding++
			}
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == '%' || r == '+' || r == '.' || r == '[' || r == ']' || r == '/':
			separator := r == '.' || r == '[' || r == ']' || r == '/' || r == '-' || r == '_' || r == '+'
			if w.padding > 0 && !separator {
				w.resetKey()
			}
			if separator {
				// A sensitive segment remains sensitive through nested names
				// such as access_token[session] or oauth.token.value.
				_, sensitive := accountCredentialKey(w.key)
				w.keyRoot = w.keyRoot || sensitive
				w.padding = 0
			}
			if r == '[' {
				w.nameDepth++
			} else if r == ']' && w.nameDepth > 0 {
				w.nameDepth--
			}
			w.key += string(r)
			if strings.EqualFold(w.key, "sk-") || w.key == "eyJ" {
				hide()
				return
			}
			root, _ := accountCredentialKey(w.key)
			w.keyRoot = w.keyRoot || root
			if len(w.key) > 128 {
				w.key = w.key[len(w.key)-128:]
			}
		default:
			w.resetKey()
		}
		visible.WriteRune(r)
	}
	w.parser.SetHandler(ansi.Handler{
		Print: printRune,
		Execute: func(b byte) {
			switch b {
			case '\n':
				w.hiding = false
				w.resetKey()
				visible.WriteByte(b)
			case '\r':
				visible.WriteByte(b)
			case '\t':
				printRune('\t')
			case '\b':
				if !w.hiding {
					if w.padding > 0 {
						w.padding--
					} else if w.key != "" {
						_, size := utf8.DecodeLastRuneInString(w.key)
						w.key = w.key[:len(w.key)-size]
					}
					visible.WriteByte(b)
				}
			}
		},
	})
	// Never forward ANSI/OSC payloads; hyperlink URIs may carry credentials.
	// The bounded parser retains controls/UTF-8 across frames. Names may stream,
	// but an assignment is classified before emitting any value bytes.
	w.parser.Parse(data)
	_, _ = io.WriteString(out, visible.String())
}

func (c *commandContext) observeAccountLogin(ctx context.Context, cancel context.CancelFunc, id string, in *os.File, out io.Writer) (<-chan error, func(), error) {
	if id == "" {
		return nil, nil, fmt.Errorf("login terminal unavailable; reconnect to inspect the operation")
	}
	cfg, err := config.Load()
	if err != nil {
		return nil, nil, err
	}
	info, err := runfile.Read(cfg.RunFilePath)
	if err != nil {
		return nil, nil, err
	}
	if info == nil || !c.deps.ProcessAlive(info.PID) {
		return nil, nil, errDaemonNotRunning
	}
	conn, response, err := websocket.Dial(ctx, fmt.Sprintf("ws://%s:%d/mux", config.LoopbackHost, info.Port), &websocket.DialOptions{HTTPClient: &http.Client{Transport: c.deps.HTTPClient.Transport}})
	if response != nil && response.Body != nil {
		defer func() { _ = response.Body.Close() }()
	}
	if err != nil {
		return nil, nil, fmt.Errorf("cannot attach login terminal")
	}
	conn.SetReadLimit(1 << 20)
	var restore func()
	if in != nil {
		old, e := term.MakeRaw(in.Fd())
		if e != nil {
			_ = conn.CloseNow()
			return nil, nil, e
		}
		restore = func() { _ = term.Restore(in.Fd(), old) }
	} else {
		restore = func() {}
	}
	attachCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	exited := make(chan error, 1)
	cleanup := func() { stop(); _ = conn.CloseNow(); <-done; restore() }
	cols, rows := 80, 24
	role := "secondary"
	if in != nil {
		role = ""
		if c, r, e := term.GetSize(in.Fd()); e == nil {
			cols, rows = c, r
		}
	}
	if cols < 1 || cols > 1000 {
		cols = 80
	}
	if rows < 1 || rows > 1000 {
		rows = 24
	}
	if err = wsjson.Write(attachCtx, conn, accountTerminalFrame{Ch: "terminal", Type: "open", ID: id, Cols: uint16(cols), Rows: uint16(rows), Role: role}); err != nil {
		stop()
		_ = conn.CloseNow()
		restore()
		return nil, nil, fmt.Errorf("cannot open login terminal")
	}
	go func() {
		defer close(done)
		var output accountLoginOutput
		for {
			var f accountTerminalFrame
			if err := wsjson.Read(attachCtx, conn, &f); err != nil {
				exited <- fmt.Errorf("login terminal connection lost")
				return
			}
			if f.ID != id || f.Ch != "terminal" {
				continue
			}
			if f.Type == "data" && out != nil {
				data, e := base64.StdEncoding.DecodeString(f.Data)
				if e != nil {
					continue
				}
				output.write(out, data)
			}
			if f.Type == "exited" {
				exited <- nil
				return
			}
			if f.Type == "error" {
				exited <- fmt.Errorf("login terminal state unavailable")
				return
			}
		}
	}()
	if in != nil {
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
	}
	return exited, cleanup, nil
}
