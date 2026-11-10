package rca

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd"
)

func TestClaudeHTTPAckBusyAndRevocationDuringNumericIO(t *testing.T) {
	for _, fault := range []string{"write", "flush"} {
		for _, control := range []string{"off", "host-stop"} {
			t.Run(fault+"/"+control, func(t *testing.T) {
				svc, _, disk, marker := claudeFlow(t, domain.KindWorker)
				entered, release := make(chan struct{}), make(chan struct{})
				var releaseOnce, enteredOnce sync.Once
				defer releaseOnce.Do(func() { close(release) })
				var writes atomic.Int64
				wait := func() { enteredOnce.Do(func() { close(entered); <-release }) }
				if fault == "write" {
					original := disk.write
					disk.write = func(f *os.File, b []byte) (int, error) { wait(); writes.Add(1); return original(f, b) }
				} else {
					original := disk.flush
					disk.flush = func(f *os.File) error { wait(); writes.Add(1); return original(f) }
				}
				router := httpd.NewRouterWithControl(config.Config{}, nil, nil, httpd.APIDeps{RCA: svc}, httpd.ControlDeps{})
				server := httptest.NewServer(router)
				defer server.Close()
				client := server.Client()
				client.Timeout = 2 * time.Second
				body := otlpBatch(otlpRecord(map[string]string{"event.name": "api_request_body", "session.id": syntheticNativeID, "request_body_id": "body1", "request_id": "r1", "body": `{"system":"A_SYNTHETIC B_SYNTHETIC"}`}))
				post := func(reader io.Reader, length int64) (*http.Response, error) {
					req, _ := http.NewRequest(http.MethodPost, server.URL+"/api/v1/rca/otlp/v1/logs", reader)
					req.ContentLength = length
					req.Header.Set("Content-Type", "application/x-protobuf")
					req.Header.Set("X-AO-RCA-Capture", marker)
					return client.Do(req)
				}
				response, err := post(bytes.NewReader(body), int64(len(body)))
				if err != nil {
					t.Fatal(err)
				}
				raw, _ := io.ReadAll(response.Body)
				_ = response.Body.Close()
				if response.StatusCode != http.StatusOK || len(raw) != 0 {
					t.Fatal("raw ACK waited for durability or leaked content")
				}
				select {
				case <-entered:
				case <-time.After(2 * time.Second):
					t.Fatal("numeric IO barrier not entered")
				}
				reader, writer := io.Pipe()
				defer reader.Close()
				defer writer.Close()
				response, err = post(reader, 1024)
				if err != nil {
					t.Fatal(err)
				}
				_, _ = io.Copy(io.Discard, response.Body)
				_ = response.Body.Close()
				if response.StatusCode != http.StatusServiceUnavailable {
					t.Fatalf("busy status=%d", response.StatusCode)
				}
				_ = writer.Close()
				path, payload := "/api/v1/sessions/selected/rca/override", `{"enabled":false}`
				if control == "host-stop" {
					path, payload = "/api/v1/rca/emergency-stop", `{"enabled":true}`
				}
				req, _ := http.NewRequest(http.MethodPut, server.URL+path, strings.NewReader(payload))
				req.Header.Set("Content-Type", "application/json")
				response, err = client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				raw, _ = io.ReadAll(response.Body)
				_ = response.Body.Close()
				if response.StatusCode != http.StatusInternalServerError || !bytes.Contains(raw, []byte("revocation_persistence_pending")) {
					t.Fatalf("durability pending hidden: status=%d body=%s", response.StatusCode, raw)
				}
				response, err = client.Get(server.URL + "/api/v1/usage/sessions/selected/composition")
				if err != nil {
					t.Fatal(err)
				}
				raw, _ = io.ReadAll(response.Body)
				_ = response.Body.Close()
				if bytes.Contains(raw, []byte(`"active":true`)) || bytes.Contains(raw, []byte("A_SYNTHETIC")) || response.StatusCode != http.StatusOK {
					t.Fatal("query blocked or returned an old active projection")
				}
				if _, why := svc.AdmitClaude(t.Context(), marker); why != "source_stopped" {
					t.Fatal("old marker admitted after withdrawal")
				}
				releaseOnce.Do(func() { close(release) })
				// Explicitly recover only after the one writer has completed its candidate
				// discard and latest revocation metadata. No retry loop performs disk writes.
				deadline := time.Now().Add(2 * time.Second)
				for {
					why := svc.core.WithStorage(t.Context(), func(context.Context) error { return nil })
					if why == "" {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("writer did not finish latest metadata")
					}
					time.Sleep(time.Millisecond)
				}
				row, ok, err := disk.LoadClaudeInline(t.Context(), "agent-orchestrator", "selected")
				if err != nil || ok || len(row.Attempts) > 0 {
					t.Fatal("late candidate became durable/current")
				}
				if writes.Load() == 0 {
					t.Fatal("barrier only simulated an immediate error")
				}
				if out := awaitClaude(t, svc); out.ClaudeInline.Active || len(out.Canonical.Events) != 1 {
					t.Fatal("late source revived or canonical control affected")
				}
			})
		}
	}
}

func TestCodexAndClaudeShareOneWriterBeforeSourceRead(t *testing.T) {
	svc, metadata, disk, _ := flowSetup(t)
	claude := metadata.sessions[0]
	claude.ID = "claude"
	claude.Harness = domain.HarnessClaudeCode
	claude.Metadata.ProviderConversationID = "22222222-2222-4222-8222-222222222222"
	metadata.sessions = append(metadata.sessions, claude)
	marker, why := svc.armClaude(t.Context(), claude)
	if why != "" {
		t.Fatal(why)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	original := disk.write
	disk.write = func(f *os.File, b []byte) (int, error) {
		enteredOnce.Do(func() { close(entered); <-release })
		return original(f, b)
	}
	if _, err := svc.Get(t.Context(), "selected", domain.RCAQuery{Analyze: true, Limit: 200}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("Codex did not enter writer barrier")
	}
	if _, why := svc.AdmitClaude(t.Context(), marker); why != "writer_busy" {
		t.Fatalf("Claude bypassed Codex writer: %s", why)
	}
	svc.mu.Lock()
	entries := svc.captureSessions[claude.ID].entries
	svc.mu.Unlock()
	if entries != 0 {
		t.Fatal("Claude parsed a body while writer was busy")
	}
	releaseOnce.Do(func() { close(release) })
	if out := completedFlow(t, svc); out.Recorded == nil || out.Status == "unavailable" {
		t.Fatal("Codex commit affected by rejected Claude admission")
	}
}

func TestClaudeLaunchGenerationRevokesReservedTicket(t *testing.T) {
	for _, role := range []domain.SessionKind{domain.KindWorker, domain.KindOrchestrator} {
		t.Run(string(role), func(t *testing.T) {
			svc, _, _, marker := claudeFlow(t, role)
			admission, why := svc.AdmitClaude(t.Context(), marker)
			if why != "" {
				t.Fatal(why)
			}
			defer admission.Release()
			receipt := svc.captureSessions["selected"].receipt
			svc.InvalidateClaudeLaunch("selected")
			if admission.Context().Err() == nil || svc.core.Current(receipt) {
				t.Fatal("launch generation did not cancel the admitted ticket")
			}
		})
	}
}
