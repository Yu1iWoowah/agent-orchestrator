package ports

import (
	"context"
	"errors"
)

// ChatCompactionOutcome separates execution evidence from RPC reply custody.
type ChatCompactionOutcome string

const (
	// ChatCompactionUnknown requires further native evidence or explicit termination.
	ChatCompactionUnknown ChatCompactionOutcome = "outcome_unknown"
	// ChatCompactionSettled identifies an observed native execution terminal.
	ChatCompactionSettled ChatCompactionOutcome = "settled"
	// ChatCompactionNotSent identifies an explicit refusal before execution.
	ChatCompactionNotSent ChatCompactionOutcome = "known_not_sent"
)

// ChatCompactionReceipt retains the original command even when its outcome is unknown.
type ChatCompactionReceipt struct {
	CommandID string
	Outcome   ChatCompactionOutcome
}

type chatCompactionKey struct{}

// WithChatCompactionID correlates one command with live native completion.
func WithChatCompactionID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, chatCompactionKey{}, id)
}

// ChatCompactionID reads the command correlation; history never carries it.
func ChatCompactionID(ctx context.Context) string {
	id, _ := ctx.Value(chatCompactionKey{}).(string)
	return id
}

// ErrChatCompactionNotSent is positive proof that no compaction request reached
// the provider. Cancellation after a write and ordinary RPC errors are unknown.
var ErrChatCompactionNotSent = errors.New("compaction was not sent")

type chatCompactionEpochKey struct{}

// WithChatCompactionEpoch preserves the original provider owner across restore.
func WithChatCompactionEpoch(ctx context.Context, epoch string) context.Context {
	return context.WithValue(ctx, chatCompactionEpochKey{}, epoch)
}

// ChatCompactionEpoch returns the provider owner preserved for this command.
func ChatCompactionEpoch(ctx context.Context) string {
	epoch, _ := ctx.Value(chatCompactionEpochKey{}).(string)
	return epoch
}
