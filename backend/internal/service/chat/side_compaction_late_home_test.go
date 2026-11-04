package chat

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/persistenthost"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type nativeHostCompactionConversation struct {
	*recordCompactionConversation
	reader ports.ChatCompactionScopeReader
}

func (c *nativeHostCompactionConversation) CompactionScope(ctx context.Context) (ports.ChatCompactionScope, error) {
	return c.reader.CompactionScope(ctx)
}

func TestSideCompactionLateHomeProviderHelper(t *testing.T) {
	if os.Getenv("PB_LATE_HOME_HELPER") != "1" {
		return
	}
	decoder, encoder := json.NewDecoder(os.Stdin), json.NewEncoder(os.Stdout)
	first, compactions := true, 0
	for {
		var request struct {
			ID     int
			Method string
		}
		if err := decoder.Decode(&request); err != nil {
			if !errors.Is(err, io.EOF) {
				os.Exit(2)
			}
			break
		}
		if first && os.Getenv("PB_CREATE_NATIVE_HOME") == "1" {
			if err := os.MkdirAll(os.Getenv("CODEX_HOME"), 0700); err != nil {
				os.Exit(2)
			}
		}
		first = false
		if request.Method == "compact" {
			compactions++
		}
		if err := encoder.Encode(map[string]any{"id": request.ID, "result": map[string]any{"compactions": compactions}}); err != nil {
			os.Exit(2)
		}
	}
	os.Exit(0)
}

func TestSideCompactionNativeHomeCreatedByFirstProviderRequest(t *testing.T) {
	for _, create := range []bool{true, false} {
		name := "created"
		flag := "1"
		if !create {
			name = "still_missing"
			flag = "0"
		}
		t.Run(name, func(t *testing.T) {
			svc, db, conv, _ := newRecordHarness(t, domain.HarnessCodex)
			root := filepath.Join(t.TempDir(), "late-native-root")
			cfg := persistenthost.Config{SessionID: "btw-run-side", DataDir: svc.dataDir, Workdir: t.TempDir(),
				Env:  append(os.Environ(), "PB_LATE_HOME_HELPER=1", "PB_CREATE_NATIVE_HOME="+flag, "CODEX_HOME="+root),
				Argv: []string{os.Args[0], "-test.run=^TestSideCompactionLateHomeProviderHelper$"}, ReconnectOnly: true}
			done := make(chan error, 1)
			go func() { done <- persistenthost.Run(context.Background(), cfg) }()
			t.Cleanup(func() {
				_ = persistenthost.Shutdown(context.Background(), cfg.DataDir, cfg.SessionID)
				if err := <-done; err != nil {
					t.Error(err)
				}
			})
			var transport *persistenthost.Transport
			deadline := time.Now().Add(3 * time.Second)
			for {
				var err error
				transport, err = persistenthost.ConnectOrStart(t.Context(), cfg)
				if err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("host attach: %v", err)
				}
				time.Sleep(10 * time.Millisecond)
			}
			defer transport.Stdin.Close()
			before, err := transport.Delivery.CompactionScope(t.Context())
			if err != nil || before.NativeHomeError == "" || before.NativeHome != "" {
				t.Fatalf("missing startup root not observed: %+v, %v", before, err)
			}
			encoder, decoder := json.NewEncoder(transport.Stdin), json.NewDecoder(transport.Stdout)
			request := func(method string) int {
				if err := encoder.Encode(map[string]any{"id": 1, "method": method}); err != nil {
					t.Fatal(err)
				}
				var response struct{ Result struct{ Compactions int } }
				if err := decoder.Decode(&response); err != nil {
					t.Fatal(err)
				}
				return response.Result.Compactions
			}
			if calls := request("first"); calls != 0 {
				t.Fatal(calls)
			}
			conv.io = func() {
				if calls := request("compact"); calls != 1 {
					t.Fatal(calls)
				}
			}
			svc.sides.runtimes["side"].conv = &nativeHostCompactionConversation{recordCompactionConversation: conv, reader: transport.Delivery}
			_, compactErr := svc.CompactSideChat(t.Context(), "session", "side")
			rows, err := db.SideCompactions(t.Context(), false)
			if err != nil {
				t.Fatal(err)
			}
			if create {
				if conv.count != 1 || len(rows) != 1 || rows[0].NativeHome != root || rows[0].NativeHomeSource != "host" {
					t.Fatalf("late root still refused: io=%d records=%+v err=%v", conv.count, rows, compactErr)
				}
				after, err := transport.Delivery.CompactionScope(t.Context())
				if err != nil || after.NativeHomeError != "" || after.NativeHome != root {
					t.Fatal(after, err)
				}
				if before.Processes != nil && (after.Processes == nil || *before.Processes != *after.Processes) {
					t.Fatal("retry replaced original process identity")
				}
			} else {
				if !errors.Is(compactErr, ErrSideCompactionNativeHomeUnavailable) || conv.count != 0 || len(rows) != 0 {
					t.Fatalf("missing root: io=%d records=%+v err=%v", conv.count, rows, compactErr)
				}
				if calls := request("stats"); calls != 0 {
					t.Fatal("refused registration sent compaction", calls)
				}
			}
		})
	}
}
