package codexappserver

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestManagedAccountRequiresFileCredentialEvidence(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux managed profile evidence")
	}
	for _, evidence := range []string{"file", "absent", "outside", "symlink", "hardlink", "public-file"} {
		t.Run(evidence, func(t *testing.T) {
			home := t.TempDir()
			if err := os.Chmod(home, 0o700); err != nil {
				t.Fatal(err)
			}
			outside := filepath.Join(t.TempDir(), "auth.json")
			if err := os.WriteFile(outside, []byte("fake-private-credential"), 0o600); err != nil {
				t.Fatal(err)
			}
			auth := filepath.Join(home, "auth.json")
			var err error
			switch evidence {
			case "file":
				err = os.WriteFile(auth, []byte("fake-private-credential"), 0o600)
			case "symlink":
				err = os.Symlink(outside, auth)
			case "hardlink":
				err = os.Link(outside, auth)
			case "public-file":
				err = os.WriteFile(auth, nil, 0o644)
			}
			if err != nil {
				t.Fatal(err)
			}
			serverReads, clientWrites := io.Pipe()
			clientReads, serverWrites := io.Pipe()
			factory := NewAccountFactory(fakePlugin{bin: "fake-codex"}, nil)
			factory.spawn = func(context.Context, string, string, []string, []string) (*process, error) {
				return &process{stdin: clientWrites, stdout: clientReads, stop: func() error {
					_ = clientWrites.Close()
					return serverWrites.Close()
				}}, nil
			}
			go serveAccountTestProtocol(serverReads, serverWrites, map[string]any{
				"initialize":   map[string]any{},
				"account/read": map[string]any{"account": map[string]any{"type": "chatgpt", "email": "fake@example.invalid"}, "requiresOpenaiAuth": true},
			})
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			client, err := factory.Open(ctx, ports.CodexAccountContext{Home: home, Managed: true})
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			_, err = client.Read(ctx, false)
			if evidence == "file" {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, ports.ErrAgentAccountCredentialStoreUnsupported) || err.Error() != "account_credential_store_unsupported" {
				t.Fatalf("missing or unsafe file evidence must fail with a path-free reason; err=%v", err)
			}
		})
	}
}
