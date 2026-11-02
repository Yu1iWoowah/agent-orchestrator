package codexappserver

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/persistenthost"
)

func TestDeliveryRecoveryProviderHelper(t *testing.T) {
	if os.Getenv("AO_DELIVERY_RECOVERY_HELPER") != "1" {
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	reads := 0
	for scanner.Scan() {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(scanner.Bytes(), &request) != nil {
			continue
		}
		response := `"result":{}`
		switch request.Method {
		case "thread/rollback":
			response = `"error":{"code":-32603,"message":"indeterminate"}`
		case "turn/steer":
			response = `"error":{"code":-32600,"message":"no active turn to steer"}`
		case "thread/read":
			reads++
			response = `"result":{"thread":{"id":"native","status":{"type":"active"},"turns":[{"id":"compact","status":"inProgress"}]}}`
			if reads > 1 {
				response = `"result":{"thread":{"id":"native","status":{"type":"idle"},"turns":[]}}`
			}
			if reads == 3 {
				response = `"error":{"code":-32603,"message":"private provider prose"}`
			}
		}
		_, _ = fmt.Fprintf(os.Stdout, `{"id":%s,%s}`+"\n", request.ID, response)
	}
	os.Exit(0)
}

func TestDeliveryRecoveryUsesOriginalHostAndFreshNativeRead(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	dataDir := t.TempDir()
	transport, err := persistenthost.ConnectOrStart(ctx, persistenthost.Config{
		SessionID: "delivery-recovery", DataDir: dataDir, Workdir: t.TempDir(),
		Env:  append(os.Environ(), "AO_DELIVERY_RECOVERY_HELPER=1"),
		Argv: []string{os.Args[0], "-test.run=^TestDeliveryRecoveryProviderHelper$"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := persistenthost.ShutdownQuick(stopCtx, dataDir, "delivery-recovery"); err != nil {
			t.Error(err)
		}
	})
	conn := newConn(transport.Stdin, transport.Stdout, slog.New(slog.DiscardHandler), nil)
	t.Cleanup(func() { _ = conn.closeWrite() })
	conv := &conversation{threadID: "native", conn: conn, proc: &process{delivery: transport.Delivery, terminate: func() error { return persistenthost.ShutdownQuick(ctx, dataDir, "delivery-recovery") }}}
	for _, method := range []string{"config/mcpServer/reload", "thread/delete"} {
		if err := conn.request(ctx, method, map[string]any{}, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := conv.FenceAndFlushOutbound(ctx, nil); err != nil {
			t.Fatalf("control permanently blocked host: %s %v", method, err)
		}
	}
	if err := conn.request(ctx, "turn/steer", map[string]any{"threadId": "native", "expectedTurnId": "finished"}, nil); err == nil {
		t.Fatal("refusal missing")
	}
	if _, err := conv.FenceAndFlushOutbound(ctx, nil); err != nil {
		t.Fatalf("known not-applied steer blocked detach: %v", err)
	}
	if err := conn.request(ctx, "thread/rollback", map[string]any{"threadId": "native"}, nil); err == nil {
		t.Fatal("unknown error missing")
	}
	if _, err := transport.Delivery.FenceAndFlushOutbound(ctx, nil); err == nil {
		t.Fatal("unknown RPC error became terminal proof")
	}
	if err := conv.ReconcileOutbound(ctx); err == nil {
		t.Fatal("native active read released unknown operation")
	}
	if err := conv.ReconcileOutbound(ctx); err != nil {
		t.Fatalf("native idle read could not recover unknown operation: %v", err)
	}
	if _, err := conv.FenceAndFlushOutbound(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := conv.Compact(ctx); err != nil {
		t.Fatal(err)
	}
	if err := conv.ReconcileOutbound(ctx); err == nil || strings.Contains(err.Error(), "private provider prose") {
		t.Fatalf("query failure released compact or exposed provider prose: %v", err)
	}
	if err := conv.ReconcileOutbound(ctx); err == nil {
		t.Fatal("idle read erased queued no-event compact")
	}
	if err := conv.Terminate(); err != nil {
		t.Fatalf("explicit stop could not release no-event compact: %v", err)
	}
}
