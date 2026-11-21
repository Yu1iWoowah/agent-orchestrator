package httpd

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/requestscope"
	"github.com/aoagents/agent-orchestrator/backend/internal/mobilebridge"
)

type rcaWireConn struct {
	net.Conn
	bytes            atomic.Int64
	failReadDeadline atomic.Bool
	closed           chan struct{}
	once             sync.Once
}

func (c *rcaWireConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	c.bytes.Add(int64(n))
	return n, err
}
func (c *rcaWireConn) SetReadDeadline(deadline time.Time) error {
	if !deadline.IsZero() && c.failReadDeadline.Load() {
		return http.ErrNotSupported
	}
	return c.Conn.SetReadDeadline(deadline)
}
func (c *rcaWireConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { close(c.closed) })
	return err
}

type rcaWireListener struct {
	net.Listener
	accepted chan *rcaWireConn
}

func (l *rcaWireListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	wire := &rcaWireConn{Conn: conn, closed: make(chan struct{})}
	l.accepted <- wire
	return wire, nil
}

type rcaBodyCounter struct {
	io.ReadCloser
	reads *atomic.Int64
}

func (b *rcaBodyCounter) Read(p []byte) (int, error) { b.reads.Add(1); return b.ReadCloser.Read(p) }

func TestRCARawOriginRejectsBeforeTail(t *testing.T) {
	svc := &rcaHTTPFixture{}
	router := NewRouterWithControl(config.Config{AllowedOrigins: []string{"app://renderer"}}, discardLogger(), nil, APIDeps{RCA: svc}, ControlDeps{})
	var bodyReads atomic.Int64
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = &rcaBodyCounter{ReadCloser: r.Body, reads: &bodyReads}
		router.ServeHTTP(w, r)
	}))
	accepted := make(chan *rcaWireConn, 1)
	server.Listener = &rcaWireListener{Listener: server.Listener, accepted: accepted}
	server.Start()
	defer server.Close()
	client, err := net.DialTimeout("tcp", server.Listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	wire := <-accepted
	defer wire.Close()
	headers := "POST /api/v1/rca/otlp/v1/logs HTTP/1.1\r\nHost: " + server.Listener.Addr().String() + "\r\nOrigin: app://renderer\r\nContent-Type: application/x-protobuf\r\nContent-Length: 1024\r\n\r\n"
	if _, err := io.WriteString(client, headers); err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	response, err := http.ReadResponse(bufio.NewReader(client), &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatal("raw rejection waited for unsent body tail")
	}
	defer response.Body.Close()
	_, _ = io.ReadAll(response.Body)
	if response.StatusCode != http.StatusForbidden {
		t.Fatal("renderer raw was not forbidden")
	}
	select {
	case <-wire.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("rejected raw connection remained reusable")
	}
	before := wire.bytes.Load()
	_ = client.SetWriteDeadline(time.Now().Add(time.Second))
	_, _ = io.WriteString(client, strings.Repeat("synthetic-tail", 100))
	if wire.bytes.Load() != before || bodyReads.Load() != 0 || svc.calls != 0 {
		t.Fatal("rejected raw consumed tail or delegated")
	}
}

type rcaStreamDeadlineFailure struct{ http.ResponseWriter }

func (w *rcaStreamDeadlineFailure) SetWriteDeadline(time.Time) error { return http.ErrNotSupported }

func TestRCARemoteHTTP2RejectPreservesSameConnection(t *testing.T) {
	for _, surface := range []string{"lan", "tunnel"} {
		for _, variant := range []string{"response", "deadline-error", "cancel"} {
			t.Run(surface+"/"+variant, func(t *testing.T) {
				siblingStarted, siblingRelease, rawStarted := make(chan struct{}), make(chan struct{}), make(chan struct{})
				var release sync.Once
				var cancelled, rawReads, delegated, hijackers atomic.Int64
				router := NewRouterWithControl(config.Config{}, discardLogger(), nil, APIDeps{}, ControlDeps{})
				router.Get("/__rca_sibling", func(w http.ResponseWriter, r *http.Request) {
					close(siblingStarted)
					select {
					case <-siblingRelease:
					case <-r.Context().Done():
						cancelled.Add(1)
						return
					}
					if r.Context().Err() != nil {
						cancelled.Add(1)
						return
					}
					_, _ = io.WriteString(w, "sibling-ok")
				})
				router.Get("/__rca_after", func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "after-ok") })
				state := &authState{}
				state.setHash(mobilebridge.HashPassword("synthetic-f0-password"))
				manager := NewLANManager(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if isRCARawRequest(r) {
						delegated.Add(1)
					}
					router.ServeHTTP(w, r)
				}), state, 0, discardLogger(), nil)
				original := manager.handler
				manager.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if isRCARawRequest(r) {
						if r.ProtoMajor != 2 {
							t.Error("raw was not HTTP2")
						}
						if _, ok := w.(http.Hijacker); ok {
							hijackers.Add(1)
						}
						r.Body = &rcaBodyCounter{ReadCloser: r.Body, reads: &rawReads}
						close(rawStarted)
						if variant == "deadline-error" {
							w = &rcaStreamDeadlineFailure{ResponseWriter: w}
						}
					}
					original.ServeHTTP(w, r)
				})
				port, err := manager.StartLoopback(0)
				if err != nil {
					t.Fatal(err)
				}
				var transport *http.Transport
				var tunnel net.Conn
				defer func() {
					release.Do(func() { close(siblingRelease) })
					if transport != nil {
						transport.CloseIdleConnections()
					}
					if tunnel != nil {
						_ = tunnel.Close()
					}
					ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
					defer cancel()
					_ = manager.Stop(ctx)
				}()
				address := fmt.Sprintf("127.0.0.1:%d", port)
				if surface == "tunnel" {
					ws, _, err := websocket.Dial(t.Context(), "ws://"+address+lanTunnelPath, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer synthetic-f0-password"}}})
					if err != nil {
						t.Fatal("synthetic tunnel upgrade failed")
					}
					tunnel = websocket.NetConn(context.Background(), ws, websocket.MessageBinary)
				}
				protocols := new(http.Protocols)
				protocols.SetUnencryptedHTTP2(true)
				var dials atomic.Int64
				transport = &http.Transport{Protocols: protocols, MaxConnsPerHost: 1, DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
					if dials.Add(1) != 1 {
						return nil, fmt.Errorf("unexpected reconnect")
					}
					if tunnel != nil {
						return tunnel, nil
					}
					return (&net.Dialer{}).DialContext(ctx, network, addr)
				}}
				client := &http.Client{Transport: transport}
				request := func(ctx context.Context, path string) (*http.Response, error) {
					req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+path, nil)
					req.Header.Set("Authorization", "Bearer synthetic-f0-password")
					return client.Do(req)
				}
				type responseResult struct {
					status int
					body   string
					err    error
				}
				normal := make(chan responseResult, 1)
				go func() {
					response, err := request(t.Context(), "/__rca_sibling")
					if err != nil {
						normal <- responseResult{err: err}
						return
					}
					defer response.Body.Close()
					body, err := io.ReadAll(response.Body)
					normal <- responseResult{status: response.StatusCode, body: string(body), err: err}
				}()
				select {
				case <-siblingStarted:
				case <-time.After(2 * time.Second):
					t.Fatal("sibling did not start")
				}
				reader, writer := io.Pipe()
				defer reader.Close()
				defer writer.Close()
				rawCtx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
				defer cancel()
				req, _ := http.NewRequestWithContext(rawCtx, http.MethodPost, "http://"+address+rcaRawPath, reader)
				req.ContentLength = 1 << 20
				req.Host = "127.0.0.1"
				req.Header.Set("Content-Type", "application/x-protobuf")
				req.Header.Set("Origin", "app://renderer")
				req.Header.Set("Authorization", "Bearer synthetic-f0-password")
				req.Header.Set("X-AO-RCA-Capture", "synthetic-not-a-bearer")
				rawResult := make(chan responseResult, 1)
				go func() {
					response, err := client.Do(req)
					if err != nil {
						rawResult <- responseResult{err: err}
						return
					}
					defer response.Body.Close()
					_, err = io.Copy(io.Discard, response.Body)
					rawResult <- responseResult{status: response.StatusCode, err: err}
				}()
				select {
				case <-rawStarted:
				case <-time.After(2 * time.Second):
					t.Fatal("raw did not enter same transport")
				}
				if variant == "cancel" {
					cancel()
				}
				select {
				case result := <-rawResult:
					if (variant == "response" && result.err != nil) || (result.err == nil && result.status != http.StatusNotFound) {
						t.Error("raw did not receive finite hidden 404")
					}
				case <-time.After(2 * time.Second):
					t.Fatal("raw rejection waited for body tail")
				}
				if rawReads.Load() != 0 || delegated.Load() != 0 || hijackers.Load() != 0 {
					t.Error("remote raw read/delegated or used HTTP1 capability")
				}
				release.Do(func() { close(siblingRelease) })
				select {
				case result := <-normal:
					if result.err != nil || result.status != 200 || result.body != "sibling-ok" || cancelled.Load() != 0 {
						t.Error("raw rejection cancelled sibling")
					}
				case <-time.After(2 * time.Second):
					t.Fatal("sibling did not complete")
				}
				response, err := request(t.Context(), "/__rca_after")
				if err != nil {
					t.Fatal("raw rejection destroyed same connection")
				}
				defer response.Body.Close()
				body, _ := io.ReadAll(response.Body)
				if response.StatusCode != http.StatusOK || string(body) != "after-ok" || dials.Load() != 1 {
					t.Fatal("follow-up required reconnect or failed")
				}
			})
		}
	}
}

func TestRCARawPartialReadReleasesAfterTerminal(t *testing.T) {
	for _, mode := range []string{"early-limit", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			router := NewRouterWithControl(config.Config{}, discardLogger(), nil, APIDeps{}, ControlDeps{})
			finished := make(chan bool, 1)
			reading := make(chan func(), 1)
			router.Post(rcaRawPath, func(w http.ResponseWriter, r *http.Request) {
				conn, _ := r.Context().Value(rcaConnKey{}).(*rcaWireConn)
				if !requestscope.OnRCARawFinished(r.Context(), func() {
					if conn == nil {
						finished <- false
						return
					}
					select {
					case <-conn.closed:
						finished <- true
					default:
						finished <- false
					}
				}) {
					t.Error("missing physical raw lifetime")
				}
				prefix := make([]byte, 4)
				if _, err := io.ReadFull(r.Body, prefix); err != nil {
					t.Error("prefix read failed")
				}
				if mode == "cancel" {
					reading <- requestscope.RCARawInterrupt(r.Context())
					tail := make([]byte, 1)
					if _, err := r.Body.Read(tail); err == nil {
						t.Error("cancel did not unblock body read")
					}
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				w.WriteHeader(http.StatusRequestEntityTooLarge)
			})
			server := httptest.NewUnstartedServer(router)
			accepted := make(chan *rcaWireConn, 1)
			server.Listener = &rcaWireListener{Listener: server.Listener, accepted: accepted}
			server.Config.ConnContext = withRCARawConn
			server.Start()
			defer server.Close()
			client, err := net.DialTimeout("tcp", server.Listener.Addr().String(), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			wire := <-accepted
			defer wire.Close()
			headers := "POST " + rcaRawPath + " HTTP/1.1\r\nHost: " + server.Listener.Addr().String() + "\r\nContent-Type: application/x-protobuf\r\nContent-Length: 1024\r\n\r\ntest"
			if _, err := io.WriteString(client, headers); err != nil {
				t.Fatal(err)
			}
			if mode == "cancel" {
				select {
				case interrupt := <-reading:
					if interrupt == nil {
						t.Fatal("missing raw interrupt")
					}
					interrupt()
				case <-time.After(time.Second):
					t.Fatal("reader did not start")
				}
			}
			_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
			response, err := http.ReadResponse(bufio.NewReader(client), &http.Request{Method: http.MethodPost})
			if err != nil {
				t.Fatal("partial raw termination waited for tail")
			}
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			select {
			case afterClose := <-finished:
				if !afterClose {
					t.Error("raw quota released before physical termination")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("raw release missing")
			}
			if response.StatusCode != http.StatusRequestEntityTooLarge && response.StatusCode != http.StatusServiceUnavailable {
				t.Fatal("partial read produced wrong fixed status")
			}
		})
	}
}

func TestRCARawHTTP1HeadersTerminateWithoutDrain(t *testing.T) {
	for _, test := range []struct {
		name, method, extra string
		length              int
		status              int
		unsupported, noRead bool
	}{
		{name: "origin", extra: "Origin: app://renderer\r\n", status: 403},
		{name: "method", method: "PUT", status: 405},
		{name: "chunked", extra: "Transfer-Encoding: chunked\r\n", length: -1, status: 411},
		{name: "encoding", extra: "Content-Encoding: gzip\r\n", status: 415},
		{name: "limit", length: (1 << 20) + 1, status: 413},
		{name: "source-off", status: 503},
		{name: "deadline-unsupported", unsupported: true},
		{name: "client-not-reading", status: 503, noRead: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var reads, delegated atomic.Int64
			router := NewRouterWithControl(config.Config{}, discardLogger(), nil, APIDeps{}, ControlDeps{})
			router.Post(rcaRawPath, func(w http.ResponseWriter, r *http.Request) {
				delegated.Add(1)
				w.Header().Set("X-Untrusted", "B_SYNTHETIC_RESPONSE")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = io.WriteString(w, "B_SYNTHETIC_RESPONSE")
			})
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				r.Body = &rcaBodyCounter{ReadCloser: r.Body, reads: &reads}
				if test.unsupported {
					if conn, ok := r.Context().Value(rcaConnKey{}).(*rcaWireConn); ok {
						conn.failReadDeadline.Store(true)
					}
				}
				router.ServeHTTP(w, r)
			}))
			accepted := make(chan *rcaWireConn, 1)
			server.Listener = &rcaWireListener{Listener: server.Listener, accepted: accepted}
			server.Config.ConnContext = withRCARawConn
			server.Start()
			defer server.Close()
			client, err := net.DialTimeout("tcp", server.Listener.Addr().String(), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			wire := <-accepted
			defer wire.Close()
			method := test.method
			if method == "" {
				method = "POST"
			}
			length := test.length
			if length == 0 {
				length = 1024
			}
			headers := method + " " + rcaRawPath + " HTTP/1.1\r\nHost: " + server.Listener.Addr().String() + "\r\nContent-Type: application/x-protobuf\r\nX-Request-ID: A_SYNTHETIC_REQUEST_HEADER\r\n" + test.extra
			if length >= 0 {
				headers += fmt.Sprintf("Content-Length: %d\r\n", length)
			}
			headers += "\r\n"
			if _, err := io.WriteString(client, headers); err != nil {
				t.Fatal(err)
			}
			if test.noRead {
				select {
				case <-wire.closed:
				case <-time.After(2 * time.Second):
					t.Fatal("nonreading client held raw connection")
				}
			}
			_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
			response, err := http.ReadResponse(bufio.NewReader(client), &http.Request{Method: method})
			if test.unsupported {
				if err == nil {
					_ = response.Body.Close()
					t.Error("unsupported terminal capability returned ordinary response")
				}
			} else {
				if err != nil {
					t.Fatal("header rejection waited for body")
				}
				body, _ := io.ReadAll(response.Body)
				_ = response.Body.Close()
				if response.StatusCode != test.status {
					t.Error("wrong fixed rejection status")
				}
				if strings.Contains(string(body), "SYNTHETIC") || strings.Contains(fmt.Sprint(response.Header), "SYNTHETIC") {
					t.Error("raw response reflected arbitrary input/output")
				}
			}
			select {
			case <-wire.closed:
			case <-time.After(2 * time.Second):
				t.Fatal("raw rejection did not terminate connection")
			}
			before := wire.bytes.Load()
			_ = client.SetWriteDeadline(time.Now().Add(time.Second))
			_, _ = io.WriteString(client, "withheld-tail")
			if reads.Load() != 0 || wire.bytes.Load() != before {
				t.Error("header rejection read or drained raw body")
			}
			want := int64(0)
			if test.name == "source-off" || test.noRead {
				want = 1
			}
			if delegated.Load() != want {
				t.Error("header rejection crossed namespace/controller boundary")
			}
		})
	}
}

func TestRCARawSuccessKeepsOrdinaryConnectionSemantics(t *testing.T) {
	router := NewRouterWithControl(config.Config{}, discardLogger(), nil, APIDeps{}, ControlDeps{})
	router.Post(rcaRawPath, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("X-Untrusted", "B_SYNTHETIC_RESPONSE")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "B_SYNTHETIC_RESPONSE")
	})
	router.Get("/__ordinary", func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ordinary-ok") })
	server := httptest.NewServer(router)
	defer server.Close()
	var dials atomic.Int64
	transport := &http.Transport{MaxConnsPerHost: 1, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		if dials.Add(1) != 1 {
			return nil, fmt.Errorf("unexpected reconnect")
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second}
	req, _ := http.NewRequest(http.MethodPost, server.URL+rcaRawPath, strings.NewReader("test"))
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("X-Request-ID", "A_SYNTHETIC_REQUEST_HEADER")
	response, err := client.Do(req)
	if err != nil {
		t.Fatal("bounded raw success failed")
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || len(body) != 0 || strings.Contains(fmt.Sprint(response.Header), "SYNTHETIC") {
		t.Fatal("raw success exposed inner diagnostics")
	}
	response, err = client.Get(server.URL + "/__ordinary")
	if err != nil {
		t.Fatal("raw timeout leaked into ordinary request")
	}
	body, _ = io.ReadAll(response.Body)
	_ = response.Body.Close()
	if string(body) != "ordinary-ok" || dials.Load() != 1 {
		t.Fatal("ordinary API changed connection behavior")
	}
}
