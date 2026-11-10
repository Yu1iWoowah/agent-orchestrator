package hcg

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/components/hcg/reporting"
)

var labName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
var labRequestID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,255}$`)
var errLabTransport = errors.New("fixed lab transport unavailable")

type labCredential struct {
	Provider  string `json:"provider"`
	Name      string `json:"name"`
	Version   string `json:"version"`
	Workspace string `json:"workspace"`
	Secret    string `json:"secret"`
}

type labHTTPProvider struct {
	root   string
	client *http.Client
}

// NewLabHTTPProvider's dependencies are trusted constructor seams, never HTTP
// or model arguments. The daemon uses its fixed OS-user store and no override.
func NewLabHTTPProvider(root string, transport http.RoundTripper) reporting.DelegationProvider {
	if transport == nil {
		transport = &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 30 * time.Second, DisableKeepAlives: true}
	}
	return &labHTTPProvider{root: root, client: &http.Client{Transport: transport, Timeout: 120 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errLabTransport }}}
}

// NewLabOwner is called only after the daemon has acquired this data root's
// exclusive lock. Creating a dormant parent does not read any credential.
func NewLabOwner(ctx context.Context, root string, store Store, ownsDataLock bool) *Service {
	if !ownsDataLock {
		return New(ctx, root, store)
	}
	u, err := user.Current()
	if err != nil || !filepath.IsAbs(u.HomeDir) {
		return New(ctx, root, store)
	}
	p := NewLabHTTPProvider(filepath.Join(u.HomeDir, ".ao", "credentials", "hcg-paid-lab"), nil)
	return NewWithDelegationProvider(ctx, root, store, p, nil)
}

func (p *labHTTPProvider) Call(ctx context.Context, r reporting.DelegationProviderRequest) (reporting.DelegationProviderResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	unsent := func() (reporting.DelegationProviderResponse, error) {
		return reporting.DelegationProviderResponse{}, errors.Join(reporting.ErrDelegationNotSent, ctx.Err())
	}
	if ctx.Err() != nil || (r.BillingProvider != "openai" && r.BillingProvider != "anthropic") || r.Endpoint != "https://api."+r.BillingProvider+".com/v1/chat/completions" || r.CredentialRef.Provider != r.BillingProvider || !labName.MatchString(r.CredentialRef.Name) || !labName.MatchString(r.CredentialRef.Version) || !labName.MatchString(r.Workspace) || r.ProfileID == "" || r.ProfileRevision == "" || r.Model == "" || len(r.Payload) > 1<<20 {
		return unsent()
	}
	var payload struct{ Model string }
	if json.Unmarshal(r.Payload, &payload) != nil || payload.Model != r.Model {
		return unsent()
	}
	raw, err := readLabVersion(ctx, p.root, r.CredentialRef)
	if err != nil {
		return unsent()
	}
	defer clear(raw)
	var credential labCredential
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&credential) != nil || decoder.Decode(new(any)) != io.EOF || credential.Provider != r.BillingProvider || credential.Name != r.CredentialRef.Name || credential.Version != r.CredentialRef.Version || credential.Workspace != r.Workspace || len(credential.Secret) == 0 || len(credential.Secret) > 4096 {
		return unsent()
	}
	for _, c := range credential.Secret {
		if c < 33 || c > 126 {
			return unsent()
		}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, r.Endpoint, bytes.NewReader(r.Payload))
	if err != nil || ctx.Err() != nil {
		return unsent()
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+credential.Secret)
	defer request.Header.Del("Authorization")
	if r.BillingProvider == "openai" {
		request.Header.Set("OpenAI-Project", credential.Workspace)
	}
	// Anthropic API credentials are workspace-scoped; the version record pins
	// that reviewed binding. No arbitrary caller header or workspace override.
	response, err := p.client.Do(request)
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}
	if err != nil {
		if ctx.Err() != nil {
			return reporting.DelegationProviderResponse{}, ctx.Err()
		}
		return reporting.DelegationProviderResponse{}, errLabTransport
	}
	safeID := func(id string) string {
		if !labRequestID.MatchString(id) || strings.Contains(id, credential.Secret) || strings.Contains(id, "HCG_CANARY_DO_NOT_SEND") {
			return ""
		}
		return id
	}
	out := reporting.DelegationProviderResponse{Status: response.StatusCode, RequestID: safeID(response.Header.Get("x-request-id")), AlternateRequestID: safeID(response.Header.Get("request-id"))}
	if response.StatusCode != http.StatusOK {
		return out, nil // Never forward a provider error body or arbitrary headers.
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(body) > 1<<20 || bytes.Contains(body, []byte(credential.Secret)) {
		return out, errLabTransport
	}
	var decoded any
	if json.Unmarshal(body, &decoded) == nil && labEchoesCredential(decoded, credential.Secret) {
		return out, errLabTransport
	}
	out.Body = body
	return out, nil
}

func labEchoesCredential(v any, secret string) bool {
	switch x := v.(type) {
	case string:
		return strings.Contains(x, secret)
	case []any:
		for _, item := range x {
			if labEchoesCredential(item, secret) {
				return true
			}
		}
	case map[string]any:
		for key, item := range x {
			if strings.Contains(key, secret) || labEchoesCredential(item, secret) {
				return true
			}
		}
	}
	return false
}
