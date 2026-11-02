package ports

import "context"

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
