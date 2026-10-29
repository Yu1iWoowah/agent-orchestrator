package agentsettings

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

const grantPurpose = "ao-agent-settings-v1"

// SettingsGrantLifetime bounds a credential injected into one actor generation.
const SettingsGrantLifetime = 24 * time.Hour

// GrantAuthority signs only settings-purpose credentials. The signing key is
// daemon-owned; neither key nor bearer belongs in responses, audit or telemetry.
type GrantAuthority struct {
	key [sha256.Size]byte
	now func() time.Time
}

// GrantKeyStore is daemon-private, with no public settings/DTO projection.
type GrantKeyStore interface {
	EnsureAgentSettingsSigningKey(context.Context, []byte) ([]byte, error)
}

// NewStoredGrantAuthority atomically reuses the daemon's private signing key.
func NewStoredGrantAuthority(ctx context.Context, store GrantKeyStore, now func() time.Time) (*GrantAuthority, error) {
	var candidate [sha256.Size]byte
	if _, err := rand.Read(candidate[:]); err != nil {
		return nil, err
	}
	key, err := store.EnsureAgentSettingsSigningKey(ctx, candidate[:])
	if err != nil {
		return nil, err
	}
	return NewGrantAuthority(key, now)
}

type grantClaims struct {
	Purpose   string                        `json:"purpose"`
	ActorID   domain.SessionID              `json:"actorId"`
	Authority domain.AgentSettingsAuthority `json:"authority"`
}

// NewGrantAuthority accepts an injected private key, copying caller-owned bytes.
func NewGrantAuthority(key []byte, now func() time.Time) (*GrantAuthority, error) {
	if len(key) != sha256.Size {
		return nil, ports.ErrAgentSettingsGrantInvalid
	}
	if now == nil {
		now = time.Now
	}
	a := &GrantAuthority{now: now}
	copy(a.key[:], key)
	return a, nil
}

func validGrantText(s string) bool {
	return s != "" && len(s) <= 256 && utf8.ValidString(s) && !strings.ContainsFunc(s, unicode.IsControl)
}

func validGrantScope(actor domain.SessionID, s domain.AgentSettingsAuthority) bool {
	return validGrantText(string(actor)) && validGrantText(string(s.ProjectID)) && validGrantText(s.ActorGeneration) && s.ActorHarness.IsKnown() && (s.ActorMode == domain.SessionModeChat || s.ActorMode == domain.SessionModeTUI) && s.IssuedAt > 0 && s.ExpiresAt > s.IssuedAt && s.ExpiresAt-s.IssuedAt <= int64(SettingsGrantLifetime/time.Second)
}

func (a *GrantAuthority) signature(payload string) []byte {
	m := hmac.New(sha256.New, a.key[:])
	_, _ = m.Write([]byte(grantPurpose + "\x00" + payload))
	return m.Sum(nil)
}

// Issue is the internal native-environment boundary, never an HTTP endpoint.
func (a *GrantAuthority) Issue(actor domain.SessionID, scope domain.AgentSettingsAuthority) (string, error) {
	if a == nil || !validGrantScope(actor, scope) || scope.IssuedAt > a.now().Unix() {
		return "", ports.ErrAgentSettingsGrantInvalid
	}
	if scope.ExpiresAt <= a.now().Unix() {
		return "", ports.ErrAgentSettingsGrantExpired
	}
	raw, err := json.Marshal(grantClaims{grantPurpose, actor, scope})
	if err != nil {
		return "", ports.ErrAgentSettingsGrantInvalid
	}
	payload := base64.RawURLEncoding.EncodeToString(raw)
	return payload + "." + base64.RawURLEncoding.EncodeToString(a.signature(payload)), nil
}

// Verify authenticates scope only. The service must still check current role,
// owner, project, target, policy and provider evidence before any mutation.
func (a *GrantAuthority) Verify(token string) (domain.SessionID, domain.AgentSettingsAuthority, error) {
	invalid := func() (domain.SessionID, domain.AgentSettingsAuthority, error) {
		return "", domain.AgentSettingsAuthority{}, ports.ErrAgentSettingsGrantInvalid
	}
	if a == nil || len(token) > 8192 {
		return invalid()
	}
	payload, mac, ok := strings.Cut(token, ".")
	if !ok || strings.Contains(mac, ".") {
		return invalid()
	}
	signature, err := base64.RawURLEncoding.Strict().DecodeString(mac)
	if err != nil || !hmac.Equal(signature, a.signature(payload)) {
		return invalid()
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(payload)
	if err != nil {
		return invalid()
	}
	var claims grantClaims
	if json.Unmarshal(raw, &claims) != nil || claims.Purpose != grantPurpose || !validGrantScope(claims.ActorID, claims.Authority) || claims.Authority.IssuedAt > a.now().Unix() {
		return invalid()
	}
	if claims.Authority.ExpiresAt <= a.now().Unix() {
		return "", domain.AgentSettingsAuthority{}, ports.ErrAgentSettingsGrantExpired
	}
	return claims.ActorID, claims.Authority, nil
}
