package terminal

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

type replacementTerminalInputLease struct{}

func (replacementTerminalInputLease) AcquireSessionInput(domain.SessionID) (func(), bool) {
	return func() {}, true
}

func (replacementTerminalInputLease) AcquireTerminalInput(context.Context, domain.SessionID) (func(), error) {
	return nil, errors.New("ORCHESTRATOR_REPLACEMENT_INTAKE_FENCED")
}

func TestTerminalReplacementAdmissionRejectsBeforePTYWriteWithNamedError(t *testing.T) {
	pty := newFakePTY()
	mgr := NewManager(&fakeSource{alive: true, spawner: &fakeSpawner{ptys: []*fakePTY{pty}}}, nil, testLogger(), WithHeartbeat(0))
	mgr.SetSessionInputLease(replacementTerminalInputLease{})
	defer mgr.Close()
	conn := newFakeConn()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mgr.Serve(ctx, conn)
	conn.in <- clientMsg{Ch: chTerminal, ID: "orchestrator", Type: msgOpen}
	recv(t, conn, chTerminal, msgOpened, time.Second)
	conn.in <- clientMsg{Ch: chTerminal, ID: "orchestrator", Type: msgData, Data: base64.StdEncoding.EncodeToString([]byte("new input\n"))}
	frame := recv(t, conn, chTerminal, msgError, time.Second)
	if !strings.Contains(frame.Error, "ORCHESTRATOR_REPLACEMENT_INTAKE_FENCED") || len(pty.writtenBytes()) != 0 {
		t.Fatalf("raw input accepted or unnamed refusal: error=%q written=%q", frame.Error, pty.writtenBytes())
	}
}
