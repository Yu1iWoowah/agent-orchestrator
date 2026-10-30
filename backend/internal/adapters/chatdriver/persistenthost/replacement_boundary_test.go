package persistenthost

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
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
	started := time.Now()
	if err := ShutdownForReplacement(ctx, dir, "cleanup-delay", "owner", false); err != nil {
		t.Errorf("positive provider-stop receipt discarded while host descriptor remains: %v", err)
	}
	if time.Since(started) < 150*time.Millisecond {
		t.Fatal("positive receipt skipped bounded host cleanup wait")
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func TestHostClientQuietAndPausedReaderRemainAttached(t *testing.T) {
	for _, mode := range []string{"quiet", "paused-reader"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			server, client := net.Pipe()
			h := &host{ctx: context.Background(), token: "token", pendingRequests: make(map[string]*pendingRequest)}
			h.cond = sync.NewCond(&h.mu)
			done := make(chan struct{})
			go func() { defer close(done); h.handle(server) }()
			t.Cleanup(func() { h.beginShutdown(); _ = server.Close(); _ = client.Close(); <-done })
			if err := json.NewEncoder(client).Encode(hello{Version: ProtocolVersion, Token: "token", Action: "attach"}); err != nil {
				t.Fatal(err)
			}
			reader := bufio.NewReader(client)
			if _, err := reader.ReadBytes('\n'); err != nil {
				t.Fatal(err)
			}
			writeDone := make(chan struct{})
			write := func() {
				h.mu.Lock()
				h.deliverProviderFrameLocked([]byte("one\n"), false)
				h.mu.Unlock()
				close(writeDone)
			}
			if mode == "paused-reader" {
				go write()
			}
			// Exceed the former production stream-write budget, not a shortened
			// test timeout. A healthy idle/paused reader is not a dead provider.
			time.Sleep(31 * time.Second)
			if mode == "quiet" {
				go write()
			}
			_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
			if frame, err := reader.ReadString('\n'); err != nil || frame != "one\n" {
				t.Fatal("healthy stream lost frame", frame, err)
			}
			<-writeDone
			h.mu.Lock()
			attached, queued := h.client != nil, len(h.detached)
			h.mu.Unlock()
			if !attached || queued != 0 {
				t.Fatal("paused reader detached or duplicated frame", attached, queued)
			}
			_ = client.Close()
			<-done
			server2, client2 := net.Pipe()
			done2 := make(chan struct{})
			go func() { defer close(done2); h.handle(server2) }()
			t.Cleanup(func() { _ = server2.Close(); _ = client2.Close(); <-done2 })
			if err := json.NewEncoder(client2).Encode(hello{Version: ProtocolVersion, Token: "token", Action: "attach"}); err != nil {
				t.Fatal(err)
			}
			reader2 := bufio.NewReader(client2)
			if _, err := reader2.ReadBytes('\n'); err != nil {
				t.Fatal(err)
			}
			go func() { h.mu.Lock(); h.deliverProviderFrameLocked([]byte("two\n"), false); h.mu.Unlock() }()
			_ = client2.SetReadDeadline(time.Now().Add(3 * time.Second))
			if frame, err := reader2.ReadString('\n'); err != nil || frame != "two\n" {
				t.Fatal("reattach duplicated prior frame", frame, err)
			}
		})
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

func TestHostAttachLaunchVerifierIsAuthenticatedAndContainsNoToken(t *testing.T) {
	for _, authenticated := range []bool{false, true} {
		server, client := net.Pipe()
		expected := domain.BrowserCapabilityVerifier("source", "launch-secret")
		h := &host{ctx: context.Background(), token: "control-secret", launchCapabilityVerifier: capabilityVerifierForEnv([]string{"AO_SESSION_ID=source", "AO_BROWSER_CAPABILITY=launch-secret"}), pendingRequests: make(map[string]*pendingRequest)}
		h.cond = sync.NewCond(&h.mu)
		done := make(chan struct{})
		go func() { defer close(done); h.handle(server) }()
		token := "invalid-token"
		if authenticated {
			token = "control-secret"
		}
		if err := json.NewEncoder(client).Encode(hello{Version: ProtocolVersion, Token: token, Action: "attach"}); err != nil {
			t.Fatal(err)
		}
		line, err := bufio.NewReader(client).ReadBytes('\n')
		if err != nil {
			t.Fatal(err)
		}
		var response helloResponse
		if err := json.Unmarshal(line, &response); err != nil {
			t.Fatal(err)
		}
		if authenticated {
			if !response.OK || response.LaunchCapabilityVerifier != expected {
				t.Fatal("launch evidence missing", response.OK)
			}
		} else if response.OK || response.LaunchCapabilityVerifier != "" {
			t.Fatal("unauthenticated launch evidence exposed")
		}
		if strings.Contains(string(line), "launch-secret") || strings.Contains(string(line), "control-secret") {
			t.Fatal("raw token exposed in host reply")
		}
		_ = client.Close()
		_ = server.Close()
		<-done
	}
}

func TestLateShutdownBeforeClientRegistrationReturnsAttached(t *testing.T) {
	server, client := net.Pipe()
	h := &host{ctx: context.Background(), token: "token", pendingRequests: make(map[string]*pendingRequest)}
	h.cond = sync.NewCond(&h.mu)
	h.admissionMu.Lock()
	var once sync.Once
	release := func() { once.Do(h.admissionMu.Unlock) }
	defer release()
	done := make(chan struct{})
	go func() { defer close(done); h.handle(server) }()
	t.Cleanup(func() { _ = server.Close(); _ = client.Close(); <-done })
	if err := json.NewEncoder(client).Encode(hello{Version: ProtocolVersion, Token: "token", Action: "attach"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for h.mu.TryLock() {
		h.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("attach never reached registration boundary")
		}
		runtime.Gosched()
	}
	// The authenticated attach passed its first check and is waiting at the
	// registration lock. Complete shutdown's state transition before admitting it.
	h.stopping.Store(true)
	release()
	_ = client.SetReadDeadline(deadline)
	var reply helloResponse
	if err := json.NewDecoder(client).Decode(&reply); err != nil || reply.OK || reply.Error != ErrAttached.Error() {
		t.Fatal("late stop returned a different refusal", reply, err)
	}
}
