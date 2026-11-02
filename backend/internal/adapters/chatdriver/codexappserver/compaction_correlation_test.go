package codexappserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestCompactionCorrelatesOnlyItsNativeTerminalEvent(t *testing.T) {
	d, srv := newTestDriver(t)
	srv.reply("thread/compact/start", `{}`)
	conv, err := d.Start(t.Context(), ports.ChatStartConfig{WorkspacePath: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conv.Close() }()
	if _, err := conv.(ports.ChatCompactor).Compact(ports.WithChatCompactionID(t.Context(), "command")); err != nil {
		t.Fatal(err)
	}
	srv.push(`{"method":"turn/completed","params":{"threadId":"thread-1","turn":{"id":"background","status":"completed"}}}`)
	if event := nextEvent(t, conv.Events(), ports.ChatEventTurnCompleted); event.CompactionID != "" {
		t.Fatalf("background completion claimed command: %+v", event)
	}
	srv.push(`{"method":"item/completed","params":{"threadId":"different-thread","turnId":"foreign","item":{"type":"contextCompaction","id":"foreign-item"}}}`)
	srv.push(`{"method":"turn/completed","params":{"threadId":"thread-1","turn":{"id":"foreign","status":"completed"}}}`)
	if event := nextEvent(t, conv.Events(), ports.ChatEventTurnCompleted); event.CompactionID != "" {
		t.Fatalf("foreign compaction claimed command: %+v", event)
	}
	srv.push(`{"method":"item/completed","params":{"threadId":"thread-1","turnId":"original","item":{"type":"contextCompaction","id":"compact-item"}}}`)
	srv.push(`{"method":"turn/completed","params":{"threadId":"thread-1","turn":{"id":"unrelated","status":"completed"}}}`)
	if event := nextEvent(t, conv.Events(), ports.ChatEventTurnCompleted); event.CompactionID != "" {
		t.Fatalf("unrelated terminal claimed known compaction: %+v", event)
	}
	srv.push(`{"method":"turn/completed","params":{"threadId":"thread-1","turn":{"id":"original","status":"completed"}}}`)
	if event := nextEvent(t, conv.Events(), ports.ChatEventTurnCompleted); event.CompactionID != "command" || event.Compaction == nil || event.Compaction.Outcome != ports.ChatCompactionSettled {
		t.Fatalf("original terminal lost command: %+v", event)
	}
	if _, err := conv.(ports.ChatCompactor).Compact(ports.WithChatCompactionID(t.Context(), "failed-command")); err != nil {
		t.Fatal(err)
	}
	srv.push(`{"method":"item/completed","params":{"threadId":"thread-1","turnId":"failed-native","item":{"type":"contextCompaction","id":"failed-item"}}}`)
	srv.push(`{"method":"turn/completed","params":{"threadId":"thread-1","turn":{"id":"failed-native","status":"failed","error":{"message":"native terminal failure"}}}}`)
	if event := nextEvent(t, conv.Events(), ports.ChatEventTurnCompleted); event.Err == nil || event.CompactionID != "failed-command" || event.Compaction == nil || event.Compaction.CommandID != "failed-command" || event.Compaction.Outcome != ports.ChatCompactionSettled {
		t.Fatalf("failed native terminal lost settlement proof: %+v", event)
	}
}

type compactionWriteFailure struct{ bytes int }

func (w compactionWriteFailure) Write([]byte) (int, error) { return w.bytes, io.ErrClosedPipe }
func (compactionWriteFailure) Close() error                { return nil }
func TestCompactionWriteEvidenceDistinguishesPartialRequest(t *testing.T) {
	for _, n := range []int{0, 1} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			reader, writer := io.Pipe()
			defer reader.Close()
			defer writer.Close()
			connection := newConn(compactionWriteFailure{bytes: n}, reader, slog.New(slog.DiscardHandler), nil)
			c := &conversation{conn: connection}
			_, err := c.Compact(ports.WithChatCompactionID(t.Context(), "command"))
			if errors.Is(err, ports.ErrChatCompactionNotSent) != (n == 0) {
				t.Fatalf("bytes=%d misclassified: %v", n, err)
			}
			if (c.compactionID == "") != (n == 0) {
				t.Fatalf("bytes=%d lost correlation: %s", n, c.compactionID)
			}
		})
	}
}
func TestCompactionCancelledBeforeWriteIsNotSent(t *testing.T) {
	c := &conversation{conn: &conn{}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := c.Compact(ports.WithChatCompactionID(ctx, "command"))
	if !errors.Is(err, context.Canceled) || !errors.Is(err, ports.ErrChatCompactionNotSent) || c.compactionID != "" {
		t.Fatalf("cancelled before write: %v id=%s", err, c.compactionID)
	}
}
func TestCompactionNativeRefusalKeepsOnlyUnknownCorrelation(t *testing.T) {
	for _, code := range []int{-32601, -32603} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			driver, server := newTestDriver(t)
			server.replyError("thread/compact/start", code, "provider reply")
			opened, err := driver.Start(t.Context(), ports.ChatStartConfig{WorkspacePath: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			defer opened.Close()
			c := opened.(*conversation)
			_, err = c.Compact(ports.WithChatCompactionID(t.Context(), "command"))
			if errors.Is(err, ports.ErrChatCompactionNotSent) != (code == -32601) {
				t.Fatalf("native refusal: %v", err)
			}
			if (c.compactionID == "") != (code == -32601) {
				t.Fatalf("unknown lost command correlation: %s", c.compactionID)
			}
		})
	}
}
