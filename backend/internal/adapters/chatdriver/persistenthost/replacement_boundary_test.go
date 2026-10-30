package persistenthost

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"sync"
	"testing"
	"time"
)

func TestAuthenticatedShutdownInterruptsBlockedReplayWithoutWaitingForMutex(t *testing.T) {
	dataServer, dataClient := net.Pipe()
	controlServer, controlClient := net.Pipe()
	h := &host{ctx: context.Background(), token: "token", pendingRequests: make(map[string]*pendingRequest), detached: [][]byte{[]byte("pending\n")}, detachedBytes: 8, shutdown: make(chan struct{})}
	h.cond = sync.NewCond(&h.mu)
	dataDone, controlDone := make(chan struct{}), make(chan struct{})
	defer func() {
		_ = dataClient.Close()
		_ = controlClient.Close()
		_ = dataServer.Close()
		_ = controlServer.Close()
		<-dataDone
		<-controlDone
	}()
	go func() { defer close(dataDone); h.handle(dataServer) }()
	if err := json.NewEncoder(dataClient).Encode(hello{Version: ProtocolVersion, Token: "token", Action: "attach"}); err != nil {
		t.Fatal(err)
	}
	if _, err := bufio.NewReader(dataClient).ReadBytes('\n'); err != nil {
		t.Fatal(err)
	}
	go func() { defer close(controlDone); h.handle(controlServer) }()
	if err := json.NewEncoder(controlClient).Encode(hello{Version: ProtocolVersion, Token: "token", Action: "shutdown-confirmed"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.shutdown:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("authenticated shutdown waited for a blocked replay writer's mutex")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stopConfirmation != controlServer {
		t.Fatal("shutdown signalled before retaining the confirmation channel")
	}
}

func TestAttachedReplayDeadlineTracksFrameProgress(t *testing.T) {
	server, client := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = client.Close() })
	h := &host{ctx: context.Background(), token: "token", pendingRequests: make(map[string]*pendingRequest), detached: [][]byte{[]byte("first\n"), []byte("second\n")}, detachedBytes: 13}
	h.cond = sync.NewCond(&h.mu)
	done := make(chan struct{})
	go func() { defer close(done); h.handle(server) }()
	if err := json.NewEncoder(client).Encode(hello{Version: ProtocolVersion, Token: "token", Action: "attach"}); err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(4 * time.Second))
	reader := bufio.NewReader(client)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var response helloResponse
	if err := json.Unmarshal(line, &response); err != nil || !response.OK {
		t.Fatal("attach refused", err)
	}
	for _, expected := range []string{"first\n", "second\n"} {
		// Each frame makes progress within the write budget; the entire replay
		// deliberately exceeds it. A cumulative attach deadline truncates it.
		time.Sleep(650 * time.Millisecond)
		frame, err := reader.ReadString('\n')
		if err != nil || frame != expected {
			t.Fatalf("progressing replay truncated: got %q error=%v", frame, err)
		}
	}
	_ = client.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("replay client did not detach")
	}
}

func TestReplacementPositiveStopReceiptSurvivesHostCleanupDelay(t *testing.T) {
	dir := t.TempDir()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	if err := writeDescriptor(dir, Descriptor{Version: ProtocolVersion, SessionID: "cleanup-delay", StopConfirmation: true, OwnershipFingerprint: "owner", Address: listener.Addr().String(), PID: os.Getpid(), Token: "token"}); err != nil {
		t.Fatal(err)
	}
	serverDone := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer conn.Close()
		var request hello
		if err := json.NewDecoder(conn).Decode(&request); err != nil {
			serverDone <- err
			return
		}
		if request.Token != "token" || request.Action != "shutdown-confirmed" {
			t.Error("unauthenticated shutdown request")
		}
		serverDone <- json.NewEncoder(conn).Encode(helloResponse{OK: true, ProviderStopped: true})
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	if err := ShutdownForReplacement(ctx, dir, "cleanup-delay", "owner", false); err != nil {
		t.Errorf("positive provider-stop receipt discarded while host descriptor remains: %v", err)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func TestUnresponsiveHostClientDoesNotBlockShutdownMutex(t *testing.T) {
	client, peer := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = peer.Close() })
	h := &host{client: client, pendingRequests: make(map[string]*pendingRequest)}
	h.activeClient.Store(&hostClient{conn: client})
	h.cond = sync.NewCond(&h.mu)
	frame := []byte("{\"result\":\"preserve this frame\"}\n")
	done := make(chan struct{})
	started := make(chan struct{})
	go func() {
		h.mu.Lock()
		close(started)
		h.deliverProviderFrameLocked(frame, false)
		h.mu.Unlock()
		close(done)
	}()
	<-started
	h.beginShutdown()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		_ = peer.Close()
		<-done
		t.Fatal("non-reading client blocked the host shutdown mutex")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.client != nil || len(h.detached) != 1 || string(h.detached[0]) != string(frame) {
		t.Fatal("shutdown-interrupted frame was not preserved for detached replay")
	}
}

func TestStoppingHostRefusesNewAttach(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	h := &host{ctx: context.Background(), token: "token", pendingRequests: make(map[string]*pendingRequest)}
	h.cond = sync.NewCond(&h.mu)
	h.beginShutdown()
	done := make(chan struct{})
	go func() { defer close(done); h.handle(server) }()
	if err := json.NewEncoder(client).Encode(hello{Version: ProtocolVersion, Token: "token", Action: "attach"}); err != nil {
		t.Fatal(err)
	}
	var response helloResponse
	if err := json.NewDecoder(client).Decode(&response); err != nil {
		t.Fatal(err)
	}
	<-done
	if response.OK || h.client != nil || h.activeClient.Load() != nil {
		t.Fatal("stopping host admitted another client")
	}
}
