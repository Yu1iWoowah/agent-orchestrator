package rca

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type claudeReadyFactsBarrier struct {
	MetadataStore
	entered, release chan struct{}
	once             sync.Once
}

func (b *claudeReadyFactsBarrier) GetSession(ctx context.Context, id domain.SessionID) (domain.SessionRecord, bool, error) {
	b.once.Do(func() {
		close(b.entered)
		select {
		case <-b.release:
		case <-ctx.Done():
		}
	})
	return b.MetadataStore.GetSession(ctx, id)
}

func TestClaudeControllerStopDuringLaunchReady(t *testing.T) {
	for _, kind := range []domain.SessionKind{domain.KindWorker, domain.KindOrchestrator} {
		for _, matching := range []bool{true, false} {
			name := "old-stop"
			if matching {
				name = "matching-stop"
			}
			t.Run(string(kind)+"/"+name, func(t *testing.T) {
				svc, metadata, _, _ := claudeFlow(t, kind)
				svc.dataRoot = t.TempDir()
				svc.SetClaudeLaunchEndpoint("http://127.0.0.1:12345/api/v1/rca/otlp/v1/logs")
				svc.InvalidateClaudeLaunch("selected")
				profile := ports.RCAClaudeRuntime{OS: "synthetic", BinaryVersion: "1", BridgeVersion: "1", SDKVersion: "1"}
				svc.claudeCapabilities = map[claudeCapability]bool{{Runtime: profile, Role: kind}: true}
				notice := ports.RCAClaudeLaunch{SessionID: "selected", ProjectID: "agent-orchestrator", Kind: kind, DataRoot: svc.dataRoot, Generation: "next-controller"}
				delta := svc.PrepareClaudeEnvironment(t.Context(), notice, profile, nil)
				marker := strings.TrimPrefix(delta["OTEL_EXPORTER_OTLP_LOGS_HEADERS"], "X-AO-RCA-Capture=")
				if marker == "" {
					t.Fatal("synthetic launch was not prepared")
				}
				metadata.sessions[0].Metadata.ControllerGeneration = notice.Generation
				barrier := &claudeReadyFactsBarrier{MetadataStore: svc.store, entered: make(chan struct{}), release: make(chan struct{})}
				svc.store = barrier
				var release sync.Once
				t.Cleanup(func() { release.Do(func() { close(barrier.release) }) })
				done := make(chan struct{})
				go func() {
					defer close(done)
					svc.ClaudeLaunchReady(t.Context(), notice, false)
				}()
				select {
				case <-barrier.entered:
				case <-time.After(2 * time.Second):
					t.Fatal("Ready did not reach facts barrier")
				}
				generation := "old-controller"
				if matching {
					generation = notice.Generation
				}
				svc.ClaudeControllerStopped("selected", generation)
				release.Do(func() { close(barrier.release) })
				select {
				case <-done:
				case <-time.After(2 * time.Second):
					t.Fatal("Ready did not finish after barrier release")
				}
				admission, why := svc.AdmitClaude(t.Context(), marker)
				if admission != nil {
					admission.Release()
				}
				if matching && why != "source_stopped" {
					t.Fatal("stopped prepared generation was armed")
				}
				if !matching && why != "" {
					t.Fatal("old stop revoked the new prepared generation")
				}
			})
		}
	}
}

func TestClaudeSafeLaunchCapabilityAndCommittedOwner(t *testing.T) {
	for _, kind := range []domain.SessionKind{domain.KindWorker, domain.KindOrchestrator} {
		t.Run(string(kind), func(t *testing.T) {
			svc, metadata, _, oldMarker := claudeFlow(t, kind)
			svc.dataRoot = t.TempDir()
			svc.SetClaudeLaunchEndpoint("http://127.0.0.1:12345/api/v1/rca/otlp/v1/logs")
			notice := ports.RCAClaudeLaunch{SessionID: "selected", ProjectID: "agent-orchestrator", Kind: kind, DataRoot: svc.dataRoot, Generation: "next-controller"}
			profile := ports.RCAClaudeRuntime{BinaryPath: "/synthetic/claude", BridgePath: "/synthetic/bridge", OS: "synthetic", BinaryVersion: "fake-1", BridgeVersion: "fake-2", SDKVersion: "fake-3"}
			svc.InvalidateClaudeLaunch("selected")
			if delta := svc.PrepareClaudeEnvironment(t.Context(), notice, profile, nil); delta != nil {
				t.Fatal("unknown native tuple got telemetry")
			}
			svc.claudeCapabilities = map[claudeCapability]bool{{Runtime: profile, Role: kind}: true}
			for _, key := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_HEADERS", "OTEL_HEADER_HELPER", "CLAUDE_CODE_ENABLE_TELEMETRY"} {
				if delta := svc.PrepareClaudeEnvironment(t.Context(), notice, profile, []string{key}); delta != nil {
					t.Fatal("existing exporter/header/helper replaced")
				}
			}
			if why := svc.claudeReason["selected"]; why != "exporter_conflict" {
				t.Fatal(why)
			}
			delta := svc.PrepareClaudeEnvironment(t.Context(), notice, profile, []string{"ANTHROPIC_API_KEY", "SYNTHETIC_NATIVE_KEY"})
			if delta["OTEL_LOGS_EXPORTER"] != "otlp" || delta["OTEL_TRACES_EXPORTER"] != "none" || delta["OTEL_METRICS_EXPORTER"] != "none" || delta["OTEL_LOG_RAW_API_BODIES"] != "1" {
				t.Fatal("missing safe exporter delta")
			}
			if _, ok := delta["ANTHROPIC_API_KEY"]; ok {
				t.Fatal("credential key copied")
			}
			marker := strings.TrimPrefix(delta["OTEL_EXPORTER_OTLP_LOGS_HEADERS"], "X-AO-RCA-Capture=")
			if marker == "" || marker == oldMarker {
				t.Fatal("namespace not rotated")
			}
			if _, why := svc.AdmitClaude(t.Context(), marker); why != "source_stopped" {
				t.Fatal("pending launch admitted raw")
			}
			metadata.sessions[0].Metadata.ControllerGeneration = notice.Generation
			svc.ClaudeLaunchReady(t.Context(), notice, false)
			admission, why := svc.AdmitClaude(t.Context(), marker)
			if why != "" {
				t.Fatal(why)
			}
			admission.Release()
			svc.ClaudeControllerStopped("selected", "old-controller")
			admission, why = svc.AdmitClaude(t.Context(), marker)
			if why != "" {
				t.Fatal("late old stop revoked current generation")
			}
			admission.Release()
			svc.ClaudeControllerStopped("selected", notice.Generation)
			if _, why := svc.AdmitClaude(t.Context(), marker); why != "source_stopped" {
				t.Fatal("stopped controller retained namespace")
			}
		})
	}
}
func TestClaudePendingLaunchCannotSurviveOffOnOrLiveReconnect(t *testing.T) {
	for _, live := range []bool{false, true} {
		t.Run(map[bool]string{false: "off-on", true: "live"}[live], func(t *testing.T) {
			svc, metadata, _, _ := claudeFlow(t, domain.KindWorker)
			svc.dataRoot = t.TempDir()
			svc.SetClaudeLaunchEndpoint("http://127.0.0.1:12345/api/v1/rca/otlp/v1/logs")
			profile := ports.RCAClaudeRuntime{OS: "synthetic", BinaryVersion: "1", BridgeVersion: "1", SDKVersion: "1"}
			svc.claudeCapabilities = map[claudeCapability]bool{{Runtime: profile, Role: domain.KindWorker}: true}
			svc.InvalidateClaudeLaunch("selected")
			notice := ports.RCAClaudeLaunch{SessionID: "selected", ProjectID: "agent-orchestrator", Kind: domain.KindWorker, DataRoot: svc.dataRoot, Generation: "next"}
			delta := svc.PrepareClaudeEnvironment(t.Context(), notice, profile, nil)
			marker := strings.TrimPrefix(delta["OTEL_EXPORTER_OTLP_LOGS_HEADERS"], "X-AO-RCA-Capture=")
			if !live {
				off, on := false, true
				if _, err := svc.SetOverride(t.Context(), "selected", &off); err != nil {
					t.Fatal(err)
				}
				if _, err := svc.SetOverride(t.Context(), "selected", &on); err != nil {
					t.Fatal(err)
				}
			}
			metadata.sessions[0].Metadata.ControllerGeneration = "next"
			svc.ClaudeLaunchReady(t.Context(), notice, live)
			if _, why := svc.AdmitClaude(t.Context(), marker); why != "source_stopped" {
				t.Fatal("old pending namespace activated")
			}
		})
	}
}
