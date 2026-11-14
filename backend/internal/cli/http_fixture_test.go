package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/aoagents/agent-orchestrator/backend/internal/daemonmeta"
)

// The binding stores server capabilities per test, never ports parsed from a
// locator. Every client captures its own server and listener lifetime.
var testHTTPBindings sync.Map // *testing.T -> *testHTTPFixture

// forbiddenFixturePort is the default daemon port. A test fixture never
// registers, requests or dials it, even when the OS hands it out.
const forbiddenFixturePort = 3001

// fixtureAddressAllowed accepts only an IPv4 loopback host:port other than the
// default daemon port. Every registration, request and dial passes this check.
func fixtureAddressAllowed(address string) bool {
	host, port, err := net.SplitHostPort(address)
	if err != nil || host != "127.0.0.1" {
		return false
	}
	value, err := strconv.Atoi(port)
	return err == nil && value > 0 && value <= 65535 && value != forbiddenFixturePort
}

var testFixtureAuditMu sync.Mutex

// recordFixtureEvent appends one JSON line to $NRR_FIXTURE_LOG when set, so an
// outer connect trace can be compared with the registered targets.
func recordFixtureEvent(owner, event, network, address string) {
	path := os.Getenv("NRR_FIXTURE_LOG")
	if path == "" {
		return
	}
	data, err := json.Marshal(map[string]any{
		"time_ns": time.Now().UnixNano(), "pid": os.Getpid(), "owner": owner,
		"event": event, "network": network, "address": address,
	})
	if err != nil {
		panic(err)
	}
	testFixtureAuditMu.Lock()
	defer testFixtureAuditMu.Unlock()
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		panic(fmt.Errorf("fixture audit: %w", err))
	}
	_, writeErr := file.Write(append(data, '\n'))
	if closeErr := file.Close(); writeErr == nil {
		writeErr = closeErr
	}
	if writeErr != nil {
		panic(fmt.Errorf("fixture audit: %w", writeErr))
	}
}

type testFixtureListener struct {
	net.Listener
	mu    sync.RWMutex
	live  bool
	owner string
}

func (l *testFixtureListener) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.live {
		recordFixtureEvent(l.owner, "revoke", "tcp", l.Addr().String())
	}
	l.live = false
	return l.Listener.Close()
}

type testHTTPFixture struct {
	server   *httptest.Server
	listener *testFixtureListener
	origin   string
	address  string
	pid      int
}

func newFixtureServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	server := httptest.NewUnstartedServer(handler)
	if !fixtureAddressAllowed(server.Listener.Addr().String()) {
		_ = server.Listener.Close()
		t.Fatalf("test fixture refused listener address %s", server.Listener.Addr())
	}
	listener := &testFixtureListener{Listener: server.Listener, live: true, owner: t.Name()}
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	bindTestHTTPFixture(t, server, 0)
	return server
}

func bindTestHTTPFixture(t *testing.T, server *httptest.Server, pid int) {
	t.Helper()
	listener, ok := server.Listener.(*testFixtureListener)
	if !ok || !fixtureAddressAllowed(listener.Addr().String()) {
		t.Fatal("HTTP fixture must own an allowed, tracked httptest.Server listener")
	}
	fixture := &testHTTPFixture{server: server, listener: listener, origin: server.URL, address: listener.Addr().String(), pid: pid}
	recordFixtureEvent(t.Name(), "register", "tcp", fixture.address)
	testHTTPBindings.Store(t, fixture)
	t.Cleanup(func() { testHTTPBindings.CompareAndDelete(t, fixture) })
}

func currentTestHTTPFixture(t *testing.T) *testHTTPFixture {
	t.Helper()
	value, _ := testHTTPBindings.Load(t)
	fixture, _ := value.(*testHTTPFixture)
	return fixture
}

func (f *testHTTPFixture) permits(req *http.Request) bool {
	if f == nil || req == nil || req.URL == nil || req.URL.User != nil || req.URL.Opaque != "" ||
		!fixtureAddressAllowed(req.URL.Host) || !fixtureAddressAllowed(f.address) {
		return false
	}
	f.listener.mu.RLock()
	defer f.listener.mu.RUnlock()
	return f.listener.live && f.server.Listener == f.listener && f.server.URL == f.origin &&
		req.URL.Scheme+"://"+req.URL.Host == f.origin && (req.Host == "" || req.Host == req.URL.Host)
}

func (f *testHTTPFixture) transport(base *http.Transport) *http.Transport {
	transport := base.Clone()
	// Real dialing is restricted independently of the requested URL, including
	// a proxy selected by the transport. No caller-provided dialer is a fallback.
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		f.listener.mu.RLock()
		defer f.listener.mu.RUnlock()
		if !fixtureAddressAllowed(address) || !f.listener.live || f.server.Listener != f.listener ||
			f.server.URL != f.origin || address != f.address ||
			(network != "tcp" && network != "tcp4" && network != "tcp6") {
			recordFixtureEvent(f.listener.owner, "reject-dial", network, address)
			return nil, errors.New("test fixture rejected dial target")
		}
		recordFixtureEvent(f.listener.owner, "allow-dial", network, address)
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	// This fixture constructor only creates plain HTTP listeners. Rejecting any
	// TLS dial also prevents bypass through a custom DialTLS, because
	// DialTLSContext takes priority when both are set.
	transport.DialTLSContext = func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("plain HTTP test fixture rejected TLS dial")
	}
	return transport
}

type testTargetGuard struct {
	fixture *testHTTPFixture
	next    http.RoundTripper
	reject  error
}

func (g *testTargetGuard) RoundTrip(req *http.Request) (*http.Response, error) {
	if !g.fixture.permits(req) {
		owner, address := "", ""
		if g.fixture != nil {
			owner = g.fixture.listener.owner
		}
		if req != nil && req.URL != nil {
			address = req.URL.Host
		}
		recordFixtureEvent(owner, "reject-request", "tcp", address)
		if g.reject != nil {
			return nil, g.reject
		}
		return nil, errors.New("test fixture rejected unowned or closed HTTP target")
	}
	return g.next.RoundTrip(req)
}

// Reconnect fixtures must explicitly retain both owned server capabilities.
// Learning a new locator never adds a target to this group.
type testFixtureClients []*testTargetGuard

func (clients testFixtureClients) RoundTrip(req *http.Request) (*http.Response, error) {
	for _, client := range clients {
		if client.fixture.permits(req) {
			return client.RoundTrip(req)
		}
	}
	return nil, errors.New("test fixture rejected target outside explicit server group")
}

type testProbeResponses struct {
	fixture *testHTTPFixture
	next    http.RoundTripper
}

func (p *testProbeResponses) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Path == "/healthz" || req.URL.Path == "/readyz" {
		if p.fixture.pid <= 0 || req.Method != http.MethodGet {
			return nil, errors.New("API fixture has no explicit daemon identity")
		}
		status := "ok"
		if req.URL.Path == "/readyz" {
			status = "ready"
		}
		return jsonResponse(http.StatusOK, fmt.Sprintf(`{"status":%q,"service":%q,"pid":%d}`, status, daemonmeta.ServiceName, p.fixture.pid)), nil
	}
	return p.next.RoundTrip(req)
}

// Terminal responses are explicit, in-memory test responders. They have no
// default transport or next delegate to fall back to.
type testTerminalResponse struct{ respond roundTripFunc }

func (m *testTerminalResponse) RoundTrip(req *http.Request) (*http.Response, error) {
	return m.respond(req)
}

type testTerminalError struct{ err error }

func (m *testTerminalError) RoundTrip(*http.Request) (*http.Response, error) { return nil, m.err }

func terminalErrorClient(err error) *http.Client {
	return &http.Client{Transport: &testTerminalError{err: err}}
}

func fixtureClient(t *testing.T, client *http.Client, synthesize bool) *http.Client {
	t.Helper()
	if client == nil {
		client = &http.Client{}
	}
	clone := *client
	switch clone.Transport.(type) {
	case *testTargetGuard, testFixtureClients:
		return &clone
	}
	fixture := currentTestHTTPFixture(t)
	var next http.RoundTripper
	var reject error
	switch transport := clone.Transport.(type) {
	case *testTerminalError:
		// An unavailable fixture returns its declared error locally. It never
		// grants authority to a locator and never calls a network delegate.
		clone.Transport = &testTargetGuard{reject: transport.err}
		return &clone
	case *testTerminalResponse:
		next = transport
	case nil:
		if base, ok := http.DefaultTransport.(*http.Transport); fixture != nil && ok {
			next = fixture.transport(base)
		}
	case *http.Transport:
		if fixture != nil {
			next = fixture.transport(transport)
		}
	default:
		reject = errors.New("opaque test transport requires an explicit terminal mock or observer")
	}
	if next == nil {
		fixture = nil
	}
	if synthesize && fixture != nil {
		next = &testProbeResponses{fixture: fixture, next: next}
	}
	clone.Transport = &testTargetGuard{fixture: fixture, next: next, reject: reject}
	return &clone
}

func rawFixtureClient(t *testing.T, client *http.Client) *http.Client {
	t.Helper()
	return fixtureClient(t, client, false)
}

func apiFixtureClient(t *testing.T, client *http.Client) *http.Client {
	t.Helper()
	return fixtureClient(t, client, true)
}

func terminalFixtureClient(t *testing.T, respond roundTripFunc) *http.Client {
	t.Helper()
	return apiFixtureClient(t, &http.Client{Transport: &testTerminalResponse{respond: respond}})
}

func observingFixtureClient(t *testing.T, client *http.Client, synthesize bool, observe func(*http.Request, http.RoundTripper) (*http.Response, error)) *http.Client {
	t.Helper()
	guarded := fixtureClient(t, client, synthesize)
	next := guarded.Transport
	clone := *guarded
	observed := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return observe(req, next)
	})
	// Preserve the input client's captured scope, including explicit groups.
	switch scope := next.(type) {
	case *testTargetGuard:
		guard := *scope
		guard.next = observed
		clone.Transport = &guard
	case testFixtureClients:
		guards := make(testFixtureClients, len(scope))
		for i, captured := range scope {
			guard := *captured
			guard.next = observed
			guards[i] = &guard
		}
		clone.Transport = guards
	default:
		t.Fatalf("observer requires a guarded fixture client, got %T", next)
	}
	return &clone
}

func guardedFixtureDeps(t *testing.T, deps Deps) Deps {
	t.Helper()
	deps = deps.withDefaults()
	deps.HTTPClient = rawFixtureClient(t, deps.HTTPClient)
	return deps
}

func newTestCommandContext(t *testing.T, deps Deps) *commandContext {
	t.Helper()
	return &commandContext{deps: guardedFixtureDeps(t, deps)}
}

func newTestRootCommand(t *testing.T, deps Deps) *cobra.Command {
	t.Helper()
	return NewRootCommand(guardedFixtureDeps(t, deps))
}

func executeWithTestDeps(t *testing.T, deps Deps, args []string) error {
	t.Helper()
	return executeWithDeps(guardedFixtureDeps(t, deps), args)
}
