package cli

import (
	"context"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd"
)

type rcaCLIFixture struct {
	enabled               *bool
	stop, purged, analyze bool
	calls                 int
}

func (s *rcaCLIFixture) Get(_ context.Context, _ domain.SessionID, q domain.RCAQuery) (domain.RCAComposition, error) {
	s.analyze = q.Analyze
	s.calls++
	return domain.RCAComposition{Status: "unavailable", Reason: "disabled"}, nil
}
func (s *rcaCLIFixture) SetOverride(_ context.Context, _ domain.SessionID, v *bool) (domain.RCAPolicyView, error) {
	s.enabled = v
	s.calls++
	return domain.RCAPolicyView{}, nil
}
func (s *rcaCLIFixture) EmergencyStop(_ context.Context, v bool) error {
	s.stop = v
	s.calls++
	return nil
}
func (s *rcaCLIFixture) Purge(context.Context, domain.SessionID) error {
	s.purged = true
	s.calls++
	return nil
}

func TestCompositionCLICallerAndValidation(t *testing.T) {
	cfg := setConfigEnv(t)
	svc := &rcaCLIFixture{}
	server := newFixtureServer(t, httpd.NewRouterWithControl(config.Config{}, nil, nil, httpd.APIDeps{RCA: svc}, httpd.ControlDeps{}))
	defer server.Close()
	writeRunFileFor(t, cfg, server)
	call := func(expected int, args ...string) {
		t.Helper()
		_, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, append([]string{"usage", "composition"}, args...)...)
		if ExitCode(err) != expected {
			t.Fatalf("composition CLI exit=%d expected=%d", ExitCode(err), expected)
		}
	}
	call(0, "get", "--session", "selected", "--analyze")
	if !svc.analyze {
		t.Fatal("analysis selection lost")
	}
	call(0, "set", "--session", "selected", "--enabled", "false")
	if svc.enabled == nil || *svc.enabled {
		t.Fatal("explicit false became inherit")
	}
	call(0, "clear", "--session", "selected")
	if svc.enabled != nil {
		t.Fatal("clear did not inherit")
	}
	call(0, "emergency-stop", "--enabled", "true")
	if !svc.stop {
		t.Fatal("host stop missing")
	}
	call(0, "purge", "--session", "selected", "--yes")
	if !svc.purged {
		t.Fatal("purge not forwarded")
	}
	before := svc.calls
	for _, args := range [][]string{{"get"}, {"get", "--session", "../x"}, {"get", "--session", "selected", "--limit", "0"}, {"get", "--session", "selected", "--after-id", "1"}, {"set", "--session", "selected"}, {"set", "--session", "selected", "--enabled", "1"}, {"purge", "--session", "selected"}, {"emergency-stop"}} {
		call(2, args...)
	}
	if svc.calls != before {
		t.Fatal("invalid CLI reached HTTP service")
	}
}
