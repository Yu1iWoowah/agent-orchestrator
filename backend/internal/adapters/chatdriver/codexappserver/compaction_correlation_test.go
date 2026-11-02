package codexappserver

import (
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
	if event := nextEvent(t, conv.Events(), ports.ChatEventTurnCompleted); event.CompactionID != "command" {
		t.Fatalf("original terminal lost command: %+v", event)
	}
}
