package delegate

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const PALCommit = "7afc7c1cc96e23992c8f105f960132c657883bb1"

var ErrUnavailable = errors.New("delegation unavailable")

type Execution struct {
	ProjectID, SessionID, DelegationID, AttemptID string
	ProjectPolicyRevision                         string
}

type Request struct {
	Question string
	Paths    []string
}
type PAL struct {
	Python, Source, TempRoot string
	diagnostics              io.Writer
}
type Consumer struct {
	PAL       PAL
	Transport Transport
	Recorder  Recorder
}
type Result struct {
	Text    string
	Receipt Receipt
}

//go:embed pal_bridge.py
var palBridge []byte

// DelegateRead is the one caller for PAL assembly and the controlled broker.
// The recorder and policy/budget gates attach here, not in a second test path.
func (c *Consumer) DelegateRead(ctx context.Context, execution Execution, profile Profile, request Request) (Result, error) {
	if runtime.GOOS != "linux" || c.Transport == nil || c.Recorder == nil || execution.ProjectID == "" || execution.SessionID == "" || execution.AttemptID == "" || request.Question == "" || !profile.valid() {
		return Result{}, ErrUnavailable
	}
	endpoint := profile.endpoint()
	if endpoint == "" || !filepath.IsAbs(c.PAL.Python) || !filepath.IsAbs(c.PAL.Source) || !filepath.IsAbs(c.PAL.TempRoot) {
		return Result{}, ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	dir, err := os.MkdirTemp(c.PAL.TempRoot, "p-")
	if err != nil {
		return Result{}, err
	}
	defer os.RemoveAll(dir)
	for _, name := range []string{"view", "cache"} {
		if err := os.Mkdir(filepath.Join(dir, name), 0700); err != nil {
			return Result{}, err
		}
	}
	paths := make([]string, 0, len(request.Paths))
	for i, path := range request.Paths {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
			return Result{}, ErrUnavailable
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return Result{}, err
		}
		name := fmt.Sprintf("s%04d.txt", i)
		if err := os.WriteFile(filepath.Join(dir, "view", name), b, 0600); err != nil {
			return Result{}, err
		}
		paths = append(paths, "/view/"+name)
	}
	bridge := filepath.Join(dir, "bridge.py")
	if err := os.WriteFile(bridge, palBridge, 0600); err != nil {
		return Result{}, err
	}
	venv := filepath.Dir(filepath.Dir(c.PAL.Python))
	args := []string{"--unshare-all", "--die-with-parent", "--new-session", "--ro-bind", "/usr", "/usr", "--ro-bind", "/lib", "/lib", "--ro-bind", "/bin", "/bin"}
	if _, err := os.Stat("/lib64"); err == nil {
		args = append(args, "--ro-bind", "/lib64", "/lib64")
	}
	args = append(args, "--proc", "/proc", "--dev", "/dev", "--dir", "/runtime", "--ro-bind", venv, "/runtime/v", "--ro-bind", c.PAL.Source, "/pal", "--ro-bind", filepath.Join(dir, "view"), "/view", "--ro-bind", bridge, "/bridge.py", "--dir", "/home", "--bind", filepath.Join(dir, "cache"), "/home/pal", "--chdir", "/pal", "--clearenv", "--setenv", "HOME", "/home/pal", "--setenv", "TMPDIR", "/home/pal", "--setenv", "PYTHONDONTWRITEBYTECODE", "1", "--setenv", "DEFAULT_MODEL", profile.Model, "--setenv", "DISABLED_TOOLS", "clink,thinkdeep,planner,consensus,codereview,precommit,debug,secaudit,docgen,analyze,refactor,tracer,testgen,challenge,apilookup", "/runtime/v/bin/python", "/bridge.py")
	cmd := exec.CommandContext(ctx, "bwrap", args...)
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	cmd.WaitDelay = 2 * time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return Result{}, err
	}
	defer stdin.Close()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, err
	}
	// Tool diagnostics remain local; no prompt/path/provider text enters errors.
	cmd.Stderr = c.PAL.diagnostics
	if cmd.Stderr == nil {
		cmd.Stderr = io.Discard
	}
	if err := cmd.Start(); err != nil {
		return Result{}, err
	}
	waited := false
	defer func() {
		cancel()
		if !waited {
			_ = cmd.Wait()
		}
	}()
	if err := json.NewEncoder(stdin).Encode(map[string]any{"profile": profile.childConfig(), "question": request.Question, "paths": paths}); err != nil {
		return Result{}, err
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4096), 2<<20)
	var result Result
	calls := 0
	for scanner.Scan() {
		var message struct {
			Kind, Prompt, System, Model string
			OK                          bool `json:"ok"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			return Result{}, ErrUnavailable
		}
		switch message.Kind {
		case "provider":
			calls++
			if calls != 1 || message.Model != profile.Model || message.System == "" || !strings.Contains(message.Prompt, request.Question) {
				return Result{}, ErrUnavailable
			}
			payload, err := json.Marshal(map[string]any{"model": profile.Model, "messages": []map[string]string{{"role": "system", "content": message.System}, {"role": "user", "content": message.Prompt}}, "max_completion_tokens": profile.OutputTokens})
			if err != nil {
				return Result{}, err
			}
			result, err = c.dispatch(ctx, execution, profile, WireRequest{Provider: profile.BillingProvider, Endpoint: endpoint, Model: profile.Model, Payload: payload})
			if err != nil {
				return Result{Receipt: result.Receipt}, err
			}
			if err := json.NewEncoder(stdin).Encode(map[string]any{"text": result.Text, "usage": map[string]int64{"input_tokens": *result.Receipt.Usage.Input, "output_tokens": *result.Receipt.Usage.Output, "total_tokens": *result.Receipt.Usage.Total}}); err != nil {
				return Result{}, err
			}
		case "done":
			if calls != 1 || !message.OK {
				return Result{}, ErrUnavailable
			}
			stdin.Close()
			waited = true
			if err := cmd.Wait(); err != nil {
				if ctx.Err() != nil {
					return Result{}, ctx.Err()
				}
				return Result{}, ErrUnavailable
			}
			return result, nil
		default:
			return Result{}, ErrUnavailable
		}
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	return Result{}, ErrUnavailable
}

// dispatch never makes cancellation depend on a cooperative transport.
// The attempt owner retains its receipt/hold even after the caller exits.
func (c *Consumer) dispatch(ctx context.Context, execution Execution, profile Profile, request WireRequest) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	type outcome struct {
		result Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		response, transportErr := c.Transport.Do(ctx, request)
		result, err := projectResponse(profile, response)
		result.Receipt.Execution = execution
		result.Receipt.ObservedAt = time.Now().UTC()
		result.Receipt.ProfileID, result.Receipt.ProfileRevision = profile.ID, profile.Revision
		result.Receipt.ProfileDigest = profile.digest()
		if transportErr != nil {
			result.Text = ""
			result.Receipt.UnknownReason = "transport_failed"
			err = transportErr
		}
		recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		recordErr := c.Recorder.Record(recordCtx, result.Receipt)
		cancel()
		if recordErr != nil {
			result.Text = ""
			err = recordErr
		}
		done <- outcome{result, err}
	}()
	select {
	case got := <-done:
		if err := ctx.Err(); err != nil {
			return Result{Receipt: got.result.Receipt}, err
		}
		return got.result, got.err
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
}
