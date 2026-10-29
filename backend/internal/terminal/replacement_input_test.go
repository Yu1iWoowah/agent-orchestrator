package terminal

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type replacementTerminalInputLease struct{}

func (replacementTerminalInputLease) AcquireSessionInput(domain.SessionID) (func(), bool) {
	return func() {}, true
}

type serialReplacementInputLease struct {
	gate   chan struct{}
	active atomic.Int32
}

func (*serialReplacementInputLease) AcquireSessionInput(domain.SessionID) (func(), bool) {
	return func() {}, true
}
func (l *serialReplacementInputLease) AcquireTerminalInput(ctx context.Context, _ domain.SessionID) (func(), error) {
	select {
	case l.gate <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	l.active.Add(1)
	return func() { l.active.Add(-1); <-l.gate }, nil
}

func TestReplacementAdmissionBufferedUntilAttachKeepsConnectionResponsive(t *testing.T) {
	pty, attached := newFakePTY(), make(chan struct{})
	src := &fakeSource{alive: true, attachFn: func(ctx context.Context, _, _ uint16) (ports.Stream, error) {
		select {
		case <-attached:
			return pty, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
	mgr := NewManager(src, nil, testLogger(), WithHeartbeat(0))
	defer mgr.Close()
	lease := &serialReplacementInputLease{gate: make(chan struct{}, 1)}
	mgr.SetSessionInputLease(lease)
	conn := newFakeConn()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mgr.Serve(ctx, conn)
	conn.in <- clientMsg{Ch: chTerminal, ID: "orchestrator", Type: msgOpen}
	conn.in <- clientMsg{Ch: chTerminal, ID: "orchestrator", Type: msgData, Data: base64.StdEncoding.EncodeToString([]byte("first"))}
	eventually(t, time.Second, func() bool { return !mgr.LastInputAt("orchestrator").IsZero() })
	if lease.active.Load() != 1 {
		t.Fatal("buffered input released its admission before attach")
	}
	conn.in <- clientMsg{Ch: chTerminal, ID: "orchestrator", Type: msgData, Data: base64.StdEncoding.EncodeToString([]byte("second"))}
	conn.in <- clientMsg{Ch: chSystem, Type: msgPing}
	recv(t, conn, chSystem, msgPong, time.Second)
	if len(pty.writtenBytes()) != 0 {
		t.Fatal("unattached input reached PTY")
	}
	close(attached)
	recv(t, conn, chTerminal, msgOpened, time.Second)
	eventually(t, time.Second, func() bool { return string(pty.writtenBytes()) == "firstsecond" && lease.active.Load() == 0 })
}

type blockingReplacementInputLease struct {
	gate    chan struct{}
	entered chan struct{}
	done    chan struct{}
	once    sync.Once
	active  atomic.Int32
}

func (*blockingReplacementInputLease) AcquireSessionInput(domain.SessionID) (func(), bool) {
	return func() {}, true
}

func (l *blockingReplacementInputLease) AcquireTerminalInput(ctx context.Context, id domain.SessionID) (func(), error) {
	if id != "orchestrator" {
		return func() {}, nil
	}
	l.once.Do(func() { close(l.entered) })
	select {
	case <-l.gate:
		l.active.Add(1)
		return func() { l.active.Add(-1) }, nil
	case <-ctx.Done():
		close(l.done)
		return nil, ctx.Err()
	}
}

func TestReplacementAdmissionWaitDoesNotBlockConnectionReadLoop(t *testing.T) {
	p1, p2 := newFakePTY(), newFakePTY()
	mgr := NewManager(&fakeSource{alive: true, spawner: &fakeSpawner{ptys: []*fakePTY{p1, p2}}}, nil, testLogger(), WithHeartbeat(0))
	defer mgr.Close()
	lease := &blockingReplacementInputLease{gate: make(chan struct{}), entered: make(chan struct{}), done: make(chan struct{})}
	mgr.SetSessionInputLease(lease)
	conn := newFakeConn()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mgr.Serve(ctx, conn)
	for _, id := range []string{"orchestrator", "worker"} {
		conn.in <- clientMsg{Ch: chTerminal, ID: id, Type: msgOpen}
		recv(t, conn, chTerminal, msgOpened, time.Second)
	}
	conn.in <- clientMsg{Ch: chTerminal, ID: "orchestrator", Type: msgData, Data: base64.StdEncoding.EncodeToString([]byte("held"))}
	select {
	case <-lease.entered:
	case <-time.After(time.Second):
		t.Fatal("admission was not attempted")
	}
	defer close(lease.gate)
	conn.in <- clientMsg{Ch: chTerminal, ID: "worker", Type: msgData, Data: base64.StdEncoding.EncodeToString([]byte("independent"))}
	conn.in <- clientMsg{Ch: chTerminal, ID: "worker", Type: msgResize, Rows: 30, Cols: 100}
	conn.in <- clientMsg{Ch: chSystem, Type: msgPing}
	recv(t, conn, chSystem, msgPong, time.Second)
	eventually(t, time.Second, func() bool { return string(p2.writtenBytes()) == "independent" && len(p2.resizeCalls()) > 0 })
	conn.in <- clientMsg{Ch: chTerminal, ID: "worker", Type: msgClose}
	select {
	case <-p2.closed:
	case <-time.After(time.Second):
		t.Fatal("other terminal close was blocked")
	}
	if len(p1.writtenBytes()) != 0 {
		t.Fatal("unadmitted input reached PTY")
	}
	// Leave admission blocked until disconnect to verify cancellation too.
	cancel()
	select {
	case <-lease.done:
	case <-time.After(time.Second):
		t.Fatal("disconnected admission did not cancel")
	}
}

func TestReplacementTerminalLeaseCoversActualPTYWrite(t *testing.T) {
	pty := newFakePTY()
	pty.writeStarted, pty.writeUnblock = make(chan struct{}), make(chan struct{})
	mgr := NewManager(&fakeSource{alive: true, spawner: &fakeSpawner{ptys: []*fakePTY{pty}}}, nil, testLogger(), WithHeartbeat(0))
	defer mgr.Close()
	lease := &blockingReplacementInputLease{gate: make(chan struct{}), entered: make(chan struct{}), done: make(chan struct{})}
	close(lease.gate)
	mgr.SetSessionInputLease(lease)
	conn := newFakeConn()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mgr.Serve(ctx, conn)
	conn.in <- clientMsg{Ch: chTerminal, ID: "orchestrator", Type: msgOpen}
	recv(t, conn, chTerminal, msgOpened, time.Second)
	conn.in <- clientMsg{Ch: chTerminal, ID: "orchestrator", Type: msgData, Data: base64.StdEncoding.EncodeToString([]byte("covered"))}
	select {
	case <-pty.writeStarted:
	case <-time.After(time.Second):
		t.Fatal("write did not start")
	}
	if lease.active.Load() != 1 {
		t.Fatal("admission released before PTY write finished")
	}
	close(pty.writeUnblock)
	eventually(t, time.Second, func() bool { return string(pty.writtenBytes()) == "covered" && lease.active.Load() == 0 })
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
