package agentsettings

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

func TestSettingsGrantSignaturePurposeAndExpiry(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	key := []byte(strings.Repeat("k", 32))
	a, err := NewGrantAuthority(key, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	scope := domain.AgentSettingsAuthority{ProjectID: "project", ActorHarness: domain.HarnessCodex, ActorMode: domain.SessionModeChat, ActorGeneration: "generation", IssuedAt: now.Unix(), ExpiresAt: now.Add(time.Hour).Unix()}
	token, err := a.Issue("orchestrator", scope)
	if err != nil {
		t.Fatal(err)
	}
	actor, got, err := a.Verify(token)
	if err != nil || actor != "orchestrator" || got != scope {
		t.Fatalf("grant scope lost: %s %+v %v", actor, got, err)
	}
	key[0] = 'x'
	if _, _, err := a.Verify(token); err != nil {
		t.Fatal("authority retained caller-owned mutable key")
	}
	other, err := NewGrantAuthority([]byte(strings.Repeat("z", 32)), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := other.Verify(token); !errors.Is(err, ports.ErrAgentSettingsGrantInvalid) {
		t.Fatalf("foreign authority token accepted: %v", err)
	}
	for _, bad := range []string{"browser-capability", "x" + token[1:], token + ".extra", strings.Repeat("x", 9000)} {
		if _, _, err := a.Verify(bad); !errors.Is(err, ports.ErrAgentSettingsGrantInvalid) {
			t.Fatalf("invalid grant accepted: %v", err)
		}
	}
	raw, _ := json.Marshal(grantClaims{Purpose: "project-documents", ActorID: "orchestrator", Authority: scope})
	payload := base64.RawURLEncoding.EncodeToString(raw)
	wrongPurpose := payload + "." + base64.RawURLEncoding.EncodeToString(a.signature(payload))
	if _, _, err := a.Verify(wrongPurpose); !errors.Is(err, ports.ErrAgentSettingsGrantInvalid) {
		t.Fatal("valid signature for another purpose accepted")
	}
	now = now.Add(time.Hour)
	if _, _, err := a.Verify(token); !errors.Is(err, ports.ErrAgentSettingsGrantExpired) {
		t.Fatalf("expired grant accepted: %v", err)
	}
	now = now.Add(-2 * time.Hour)
	if _, _, err := a.Verify(token); !errors.Is(err, ports.ErrAgentSettingsGrantInvalid) {
		t.Fatalf("future-issued grant trusted: %v", err)
	}
}

func TestSettingsGrantAuthoritySurvivesDaemonReplacement(t *testing.T) {
	s := sqlitetest.MustOpenAt(t, t.TempDir())
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	a, err := NewStoredGrantAuthority(t.Context(), s, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	scope := domain.AgentSettingsAuthority{ProjectID: "project", ActorHarness: domain.HarnessCodex, ActorMode: domain.SessionModeChat, ActorGeneration: "generation", IssuedAt: now.Unix(), ExpiresAt: now.Add(time.Hour).Unix()}
	token, err := a.Issue("orchestrator", scope)
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := NewStoredGrantAuthority(t.Context(), s, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	actor, got, err := replacement.Verify(token)
	if err != nil || actor != "orchestrator" || got != scope {
		t.Fatalf("daemon replacement invalidated live grant: %s %+v %v", actor, got, err)
	}
}

func TestSettingsGrantRejectsIncompleteOrUnboundedScope(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	if _, err := NewGrantAuthority([]byte("short"), nil); err == nil {
		t.Fatal("short signing key accepted")
	}
	a, err := NewGrantAuthority([]byte(strings.Repeat("k", 32)), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	valid := domain.AgentSettingsAuthority{ProjectID: "project", ActorHarness: domain.HarnessCodex, ActorMode: domain.SessionModeChat, ActorGeneration: "generation", IssuedAt: now.Unix(), ExpiresAt: now.Add(time.Hour).Unix()}
	for _, change := range []func(*domain.AgentSettingsAuthority){
		func(s *domain.AgentSettingsAuthority) { s.ProjectID = "" },
		func(s *domain.AgentSettingsAuthority) { s.ActorGeneration = "" },
		func(s *domain.AgentSettingsAuthority) { s.ActorGeneration = "unsafe\ngeneration" },
		func(s *domain.AgentSettingsAuthority) { s.ActorHarness = "unknown" },
		func(s *domain.AgentSettingsAuthority) { s.ActorMode = "unknown" },
		func(s *domain.AgentSettingsAuthority) { s.ExpiresAt = s.IssuedAt },
		func(s *domain.AgentSettingsAuthority) { s.ExpiresAt = now.Add(8 * 24 * time.Hour).Unix() },
	} {
		scope := valid
		change(&scope)
		if _, err := a.Issue("orchestrator", scope); !errors.Is(err, ports.ErrAgentSettingsGrantInvalid) {
			t.Fatalf("invalid grant scope accepted: %+v %v", scope, err)
		}
	}
	if _, err := a.Issue("", valid); !errors.Is(err, ports.ErrAgentSettingsGrantInvalid) {
		t.Fatalf("unidentified actor accepted: %v", err)
	}
}
