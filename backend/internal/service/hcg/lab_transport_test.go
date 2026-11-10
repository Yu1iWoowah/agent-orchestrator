package hcg

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/components/hcg/reporting"
)

const labFixtureMarker = "SYNTHETIC_TRANSPORT_MARKER"

type labRoundTripper func(*http.Request) (*http.Response, error)

func (f labRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func labCredentialFixture(t *testing.T, family string) (string, string) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("lab requires Linux isolation")
	}
	root := t.TempDir()
	for _, path := range []string{root, filepath.Join(root, family), filepath.Join(root, family, "synthetic-ref")} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	// This is a noncredential marker in a private fixture, never a real key.
	raw, _ := json.Marshal(map[string]string{"provider": family, "name": "synthetic-ref", "version": "v1", "workspace": "synthetic-workspace", "secret": labFixtureMarker})
	path := filepath.Join(root, family, "synthetic-ref", "v1.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return root, path
}

func TestLabDefaultHTTPVerifiesTLSAndIgnoresProxy(t *testing.T) {
	root, _ := labCredentialFixture(t, "openai")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("untrusted TLS peer received an HTTP request")
	}))
	defer server.Close()
	p := NewLabHTTPProvider(root, nil).(*labHTTPProvider)
	transport := p.client.Transport.(*http.Transport)
	dials := 0
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		dials++
		if address != "api.openai.com:443" {
			t.Error("transport used environment proxy instead of fixed host")
		}
		// Only this private loopback listener is dialed; no provider or DNS call.
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	out, err := p.Call(t.Context(), labRequest("openai"))
	if err == nil || dials != 1 || len(out.Body) != 0 || strings.Contains(err.Error(), labFixtureMarker) {
		t.Fatal("TLS failure was retried, accepted, or exposed unsafe data")
	}
}

func labRequest(family string) reporting.DelegationProviderRequest {
	return reporting.DelegationProviderRequest{BillingProvider: family, ProfileID: family + "-fixture", ProfileRevision: "v1", Model: "fixture-model", Endpoint: "https://api." + family + ".com/v1/chat/completions", Workspace: "synthetic-workspace", CredentialRef: reporting.DelegationCredentialReference{Provider: family, Name: "synthetic-ref", Version: "v1"}, Payload: []byte(`{"model":"fixture-model","max_completion_tokens":64,"messages":[{"role":"system","content":"fixed"},{"role":"user","content":"synthetic"}]}`)}
}

func TestLabTransportFixedFamiliesAndSafeProjection(t *testing.T) {
	for _, family := range []string{"openai", "anthropic"} {
		t.Run(family, func(t *testing.T) {
			root, _ := labCredentialFixture(t, family)
			calls := 0
			provider := NewLabHTTPProvider(root, labRoundTripper(func(r *http.Request) (*http.Response, error) {
				calls++
				body, _ := io.ReadAll(r.Body)
				if r.URL.String() != labRequest(family).Endpoint || r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer "+labFixtureMarker || strings.Contains(string(body), labFixtureMarker) {
					t.Error("fixed endpoint/auth/payload boundary failed")
				}
				if family == "openai" && r.Header.Get("OpenAI-Project") != "synthetic-workspace" {
					t.Error("project accounting binding missing")
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"X-Request-Id": {"synthetic-request"}, "Set-Cookie": {labFixtureMarker}}, Body: io.NopCloser(strings.NewReader(`{"model":"fixture-model","usage":{"prompt_tokens":4,"completion_tokens":1,"total_tokens":5}}`)), Request: r}, nil
			}))
			out, err := provider.Call(t.Context(), labRequest(family))
			if err != nil || calls != 1 || out.Status != 200 || out.RequestID != "synthetic-request" || strings.Contains(string(out.Body), labFixtureMarker) {
				t.Fatal("fixed transport did not return safe raw usage once")
			}
		})
	}
}

func TestLabResolverRejectsBeforeHTTP(t *testing.T) {
	for _, name := range []string{"family", "version", "workspace", "endpoint", "file_mode", "directory_mode", "symlink"} {
		t.Run(name, func(t *testing.T) {
			root, path := labCredentialFixture(t, "openai")
			r := labRequest("openai")
			switch name {
			case "family":
				r.CredentialRef.Provider = "anthropic"
			case "version":
				r.CredentialRef.Version = "v2"
			case "workspace":
				r.Workspace = "other-workspace"
			case "endpoint":
				r.Endpoint = "https://example.invalid/"
			case "file_mode":
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			case "directory_mode":
				if err := os.Chmod(filepath.Dir(path), 0755); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Rename(path, path+".saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".saved", path); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			p := NewLabHTTPProvider(root, labRoundTripper(func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("unexpected HTTP") }))
			out, err := p.Call(t.Context(), r)
			if err == nil || !errors.Is(err, reporting.ErrDelegationNotSent) || calls != 0 || len(out.Body) != 0 || strings.Contains(err.Error(), labFixtureMarker) {
				t.Fatal("resolver refusal reached HTTP or exposed unsafe output")
			}
		})
	}
}

func TestLabHTTPRejectsRedirectAndSecretEcho(t *testing.T) {
	for _, mode := range []string{"redirect", "error_body", "decoded_echo"} {
		t.Run(mode, func(t *testing.T) {
			root, _ := labCredentialFixture(t, "openai")
			calls := 0
			p := NewLabHTTPProvider(root, labRoundTripper(func(r *http.Request) (*http.Response, error) {
				calls++
				status, body := 200, `{"echo":"\u0053YNTHETIC_TRANSPORT_MARKER"}`
				if mode == "redirect" {
					status = 302
				}
				if mode == "error_body" {
					status, body = 401, labFixtureMarker
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Location": {"https://example.invalid/"}, "X-Request-Id": {labFixtureMarker}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			}))
			out, err := p.Call(t.Context(), labRequest("openai"))
			if calls != 1 || len(out.Body) != 0 || out.RequestID != "" || (err != nil && strings.Contains(err.Error(), labFixtureMarker)) {
				t.Fatal("redirect/retry or secret projection escaped")
			}
		})
	}
}

func TestLabHTTPInFlightCancellation(t *testing.T) {
	root, _ := labCredentialFixture(t, "openai")
	entered := make(chan struct{})
	p := NewLabHTTPProvider(root, labRoundTripper(func(r *http.Request) (*http.Response, error) {
		close(entered)
		<-r.Context().Done()
		return nil, r.Context().Err()
	}))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { _, err := p.Call(ctx, labRequest("openai")); done <- err }()
	<-entered
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("cancellation classification lost")
		}
	case <-time.After(time.Second):
		t.Fatal("transport did not stop after caller cancellation")
	}
}
