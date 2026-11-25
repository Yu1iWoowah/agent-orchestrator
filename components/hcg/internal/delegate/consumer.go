package delegate

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

var ErrUnavailable = errors.New("delegation unavailable")

type Execution struct {
	ProjectID, SessionID, DelegationID, AttemptID string
	ProjectPolicyRevision                         string
}

type Request struct {
	Question    string
	Focus       string
	OutputLimit int
	Paths       []string
}
type Consumer struct {
	checkpoint    func(context.Context, string)
	logger        *slog.Logger
	ApprovedScope ApprovedScope
	Transport     Transport
	Recorder      Recorder
}
type Result struct {
	Reply   ReadReply
	Audit   PayloadAudit
	sources preparedScope
	text    string
	Receipt Receipt
}

// DelegateRead is the one caller for payload assembly and the controlled broker.
// The recorder and policy/budget gates attach here, not in a second test path.
func (c *Consumer) DelegateRead(ctx context.Context, execution Execution, profile Profile, request Request) (Result, error) {
	if c.Transport == nil || c.Recorder == nil || execution.ProjectID == "" || execution.SessionID == "" || execution.AttemptID == "" || request.Question == "" || !profile.valid() {
		return Result{}, ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	if len(request.Focus) > 4096 || !safeText(request.Focus) || request.OutputLimit < 0 || request.OutputLimit > 65536 {
		return Result{}, ErrUnavailable
	}
	scope, err := prepareScope(ctx, c.ApprovedScope, request.Question, request.Paths, profile.ContextTokens-profile.OutputTokens)
	if err != nil {
		return Result{}, err
	}
	if err := c.atCheckpoint(ctx, "assembly"); err != nil {
		return Result{}, err
	}
	payload, audit, err := buildPayload(profile, questionPrompt(request.Question, request.Focus, scope), scope)
	audit.QuestionHash = textHash(request.Question)
	audit.InstructionHash = textHash(responseInstructions)
	audit.FocusHash = textHash(request.Focus)
	if err != nil {
		return Result{Audit: audit}, err
	}
	result, err := c.dispatch(ctx, execution, profile, WireRequest{Provider: profile.BillingProvider, Endpoint: profile.endpoint(), Model: profile.Model, Payload: payload, ScopeDigest: audit.ScopeDigest})
	result.Audit, result.sources = audit, scope
	if err != nil {
		return Result{Receipt: result.Receipt, Audit: result.Audit}, err
	}
	if err := c.atCheckpoint(ctx, "answer_validation"); err != nil {
		return Result{Receipt: result.Receipt}, err
	}
	reply, failure, err := verifyReply(result.Receipt.ObservedAt, result.text, execution, profile, result.Receipt, result.Audit, scope, request.OutputLimit)
	result.text = ""
	if failure != nil && c.logger != nil {
		c.logger.WarnContext(ctx, "HCG delegation reply rejected", "stage", failure.Stage, "code", failure.Code, "attempt_id", execution.AttemptID)
	}
	if cancelErr := ctx.Err(); cancelErr != nil {
		return Result{Receipt: result.Receipt}, cancelErr
	}
	result.Reply = reply
	return result, err
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
			result.text = ""
			if result.Receipt.UnknownReason != "" {
				result.Receipt.UnknownReason = "transport_failed"
			}
			err = transportErr
		}
		recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		recordErr := c.Recorder.Record(recordCtx, result.Receipt)
		cancel()
		if recordErr != nil {
			result.text = ""
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

// Fixed constructor-only fault points share the caller's cancellation bound.
// Production has no checkpoint; no HTTP/tool input can install one.
func (c *Consumer) atCheckpoint(ctx context.Context, point string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.checkpoint == nil {
		return nil
	}
	done := make(chan struct{})
	go func() { c.checkpoint(ctx, point); close(done) }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
		return ctx.Err()
	}
}
