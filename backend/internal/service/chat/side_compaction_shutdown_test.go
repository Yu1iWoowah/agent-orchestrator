//go:build linux

package chat

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/processscope"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

// This helper owns and reaps its provider group before exiting. No provider
// credentials or persistent host are used by these process-boundary tests.
func TestHistoricalScopeHostHelper(t *testing.T) {
	if os.Getenv("PB_SCOPE_HELPER") != "1" {
		return
	}
	child := exec.Command("sleep", "60")
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := child.Start(); err != nil {
		os.Exit(2)
	}
	scope := processscope.Capture(child.Process.Pid)
	if err := json.NewEncoder(os.Stdout).Encode(scope); err != nil {
		os.Exit(3)
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
	_ = child.Process.Kill()
	_ = child.Wait()
	os.Exit(0)
}

func historicalScopeHost(t *testing.T) (*domain.CompactionProcessScope, func()) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHistoricalScopeHostHelper$")
	cmd.Env = append(os.Environ(), "PB_SCOPE_HELPER=1")
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		_ = input.Close()
		if err := cmd.Wait(); err != nil {
			t.Errorf("scope host: %v", err)
		}
	}
	t.Cleanup(stop)
	var scope *domain.CompactionProcessScope
	if err := json.NewDecoder(output).Decode(&scope); err != nil || scope == nil {
		t.Fatalf("scope: %v %v", scope, err)
	}
	return scope, stop
}

func historicalSide(t *testing.T, scope *domain.CompactionProcessScope, provider domain.AgentHarness, mode domain.PermissionMode) (*Service, *sqlite.Store, *recordCompactionConversation) {
	t.Helper()
	svc, db, conv, home := newRecordHarness(t, provider)
	runtime := svc.sides.runtimes["side"]
	var frozen frozenSideConfig
	if err := json.Unmarshal(runtime.side.LaunchConfig, &frozen); err != nil {
		t.Fatal(err)
	}
	frozen.Permissions = mode
	frozen.WorkspacePath = home
	runtime.side.LaunchConfig, _ = json.Marshal(frozen)
	// The provider home is explicitly supplied, including the unmanaged case.
	runtime.conv = &reviewScopeConversation{recordCompactionConversation: conv, scope: ports.ChatCompactionScope{NativeHome: home, Processes: scope}}
	_, _ = svc.CompactSideChat(t.Context(), "session", "side")
	rows, err := db.SideCompactions(t.Context(), true)
	if err != nil || len(rows) != 1 {
		t.Fatalf("register: %+v %v", rows, err)
	}
	return svc, db, conv
}

func onlyCompaction(t *testing.T, db *sqlite.Store) domain.SideCompaction {
	t.Helper()
	rows, err := db.SideCompactions(t.Context(), false)
	if err != nil || len(rows) != 1 {
		t.Fatalf("records: %+v %v", rows, err)
	}
	return rows[0]
}

func TestHistoricalUnknownShutdown(t *testing.T) {
	for _, boundary := range []string{"esrch", "boot_changed", "pending_at_crash"} {
		t.Run(boundary, func(t *testing.T) {
			scope, stop := historicalScopeHost(t)
			if boundary == "boot_changed" {
				scope.BootID = "previous-boot"
			}
			svc, db, conv := historicalSide(t, scope, domain.HarnessCodex, domain.PermissionModeAcceptEdits)
			before := onlyCompaction(t, db)
			if boundary == "pending_at_crash" {
				writer := recordWriter(t, svc)
				if _, err := writer.Exec(`UPDATE chat_side_compactions SET state='pending'`); err != nil {
					t.Fatal(err)
				}
			}
			stop()
			// Include accepted intake, held state and attachment references in the flush.
			store := svc.sides.store.(*memorySideStore)
			if err := store.SetSideDraft(t.Context(), "side", "run", receiptDraft, time.Now()); err != nil {
				t.Fatal(err)
			}
			if err := svc.PreflightShutdown(t.Context()); err != nil {
				t.Fatal(err)
			}
			after := onlyCompaction(t, db)
			if after.State != domain.SideCompactionUnknown || !after.Historical || after.Closed || after.ScopeEndReason == "" || !bytes.Contains(after.Content, []byte("draft before")) {
				t.Fatalf("historical result: %+v content=%s", after, after.Content)
			}
			if before.ID != after.ID || !reflect.DeepEqual(svc.ShutdownPreservedSides(), []string{"side"}) {
				t.Fatalf("report: %v", svc.ShutdownPreservedSides())
			}
			runtime := svc.sides.runtimes["side"]
			runtime.mu.Lock()
			isolated := runtime.outboundUnverified && runtime.compactionID == after.ID
			runtime.mu.Unlock()
			if !isolated || conv.count != 1 {
				t.Fatalf("isolation=%v calls=%d", isolated, conv.count)
			}
			restarted := New(Options{Store: db, DataDir: svc.dataDir, AppRunID: "after", Sessions: sideSessionReader{}})
			if err := restarted.PreflightShutdown(t.Context()); err != nil {
				t.Fatalf("restart: %v", err)
			}
			if !reflect.DeepEqual(restarted.ShutdownPreservedSides(), []string{"side"}) {
				t.Fatal("historical side absent after restart")
			}
		})
	}
}

func TestHistoricalUnknownShutdownRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		change       func(*domain.CompactionProcessScope)
		provider     domain.AgentHarness
		mode         domain.PermissionMode
	}{
		{name: "live host and idle protocol", reason: "scope_host_alive"},
		{name: "namespace", reason: "scope_namespace_mismatch", change: func(s *domain.CompactionProcessScope) { s.PIDNamespace = "different" }},
		{name: "missing identity", reason: "scope_identity_missing", change: func(s *domain.CompactionProcessScope) { s.LeaderBirth = "" }},
		{name: "Codex full access", reason: "codex_full_access", provider: domain.HarnessCodex, mode: domain.PermissionModeBypassPermissions},
		{name: "Codex default full access", reason: "codex_full_access", provider: domain.HarnessCodex, mode: domain.PermissionModeDefault},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scope, _ := historicalScopeHost(t)
			if tc.change != nil {
				tc.change(scope)
			}
			provider := tc.provider
			if provider == "" {
				provider = domain.HarnessClaudeCode
			}
			svc, db, conv := historicalSide(t, scope, provider, tc.mode)
			_ = conv // No original normal result is available; scope liveness decides the veto.
			err := svc.PreflightShutdown(t.Context())
			var sideErr *ports.ChatShutdownSideError
			if !errors.Is(err, ports.ErrChatShutdownNotSettled) || !errors.As(err, &sideErr) || sideErr.SideID != "side" || sideErr.Reason != tc.reason {
				t.Fatalf("%v", err)
			}
			if onlyCompaction(t, db).Historical {
				t.Fatal("negative scope became historical")
			}
		})
	}
}

func TestHistoricalApplicability(t *testing.T) {
	for _, tc := range []struct {
		name, platform    string
		provider          domain.AgentHarness
		mode              domain.PermissionMode
		readOnly, allowed bool
	}{
		{name: "Claude unmanaged no sandbox", platform: "linux", provider: domain.HarnessClaudeCode, allowed: true},
		{name: "Codex writable native home", platform: "linux", provider: domain.HarnessCodex, mode: domain.PermissionModeAcceptEdits, allowed: true},
		{name: "Codex auto", platform: "linux", provider: domain.HarnessCodex, mode: domain.PermissionModeAuto, allowed: true},
		{name: "Codex read only override", platform: "linux", provider: domain.HarnessCodex, readOnly: true, allowed: true},
		{name: "macOS", platform: "darwin", provider: domain.HarnessClaudeCode},
		{name: "Windows", platform: "windows", provider: domain.HarnessClaudeCode},
		{name: "Codex unspecified", platform: "linux", provider: domain.HarnessCodex},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := domain.SideCompaction{Provider: tc.provider, PermissionMode: tc.mode, ReadOnly: tc.readOnly}
			allowed, _ := sideCompactionApplicable(row, tc.platform)
			if allowed != tc.allowed {
				t.Fatalf("allowed=%v", allowed)
			}
			if !allowed {
				return
			}
			if tc.readOnly {
				return
			}
			scope, stop := historicalScopeHost(t)
			svc, _, _ := historicalSide(t, scope, tc.provider, tc.mode)
			// // Original launch override is covered by the applicability assertion.
			stop()
			if err := svc.PreflightShutdown(t.Context()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestHistoricalUnknownLateResultsPreserveContent(t *testing.T) {
	scope, stop := historicalScopeHost(t)
	svc, db, conv := historicalSide(t, scope, domain.HarnessClaudeCode, domain.PermissionModeDefault)
	store := svc.sides.store.(*memorySideStore)
	turn := domain.SideTurn{ID: "held", SideID: "side", ClientMessageID: "client", Text: "held intake", CreatedAt: time.Now()}
	if _, _, err := store.ReserveSideTurn(t.Context(), turn, "run"); err != nil {
		t.Fatal(err)
	}
	stop()
	if err := svc.PreflightShutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	before := onlyCompaction(t, db)
	runtime := svc.sides.runtimes["side"]
	event := ports.ChatEvent{ProviderTurnID: "original", ProviderEventID: "late", CompactionID: before.ID, Compaction: &ports.ChatCompactionReceipt{CommandID: before.ID, Outcome: ports.ChatCompactionSettled}}
	if svc.sides.settleCompactionResult(t.Context(), runtime, event) {
		t.Fatal("late original result settled")
	}
	if err := db.TransitionSideCompaction(t.Context(), before.ID, "replacement-epoch", domain.SideCompactionSettled, false, false); !errors.Is(err, domain.ErrSideCompactionTransition) {
		t.Fatal(err)
	}
	svc.sides.clearSettledAttention(t.Context())
	after := onlyCompaction(t, db)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("late observation changed record/content: before=%+v after=%+v", before, after)
	}
	if conv.count != 1 {
		t.Fatal("compaction replayed")
	}
}

func TestHistoricalUnknownExplicitCloseAcrossRestart(t *testing.T) {
	for _, via := range []string{"side endpoint", "global orphan"} {
		t.Run(via, func(t *testing.T) {
			scope, stop := historicalScopeHost(t)
			svc, db, _ := historicalSide(t, scope, domain.HarnessCodex, domain.PermissionModeBypassPermissions)
			stop()
			if err := svc.PreflightShutdown(t.Context()); !errors.Is(err, ports.ErrChatShutdownNotSettled) {
				t.Fatal(err)
			}
			if via == "global orphan" {
				svc = New(Options{Store: db, DataDir: svc.dataDir, AppRunID: "new-launch", Sessions: sideSessionReader{}})
				if err := svc.CloseProtectedSide(t.Context(), "side"); err != nil {
					t.Fatal(err)
				}
			} else if err := svc.ExplicitlyCloseSideCompaction(t.Context(), "session", "side"); err != nil {
				t.Fatal(err)
			}
			restarted := New(Options{Store: db, DataDir: svc.dataDir, AppRunID: "restart", Sessions: sideSessionReader{}})
			if err := restarted.PreflightShutdown(t.Context()); err != nil {
				t.Fatal(err)
			}
			row := onlyCompaction(t, db)
			if !row.Closed || row.State != domain.SideCompactionUnknown || len(row.Content) != 0 || row.NativeID == "" {
				t.Fatalf("closed metadata: %+v", row)
			}
		})
	}
}

func TestHistoricalUnknownMultipleSidesAndDurabilityFailure(t *testing.T) {
	scope, stop := historicalScopeHost(t)
	svc, db, _ := historicalSide(t, scope, domain.HarnessClaudeCode, domain.PermissionModeDefault)
	stop()
	row := onlyCompaction(t, db)
	row.ID, row.SideID, row.NativeID = "second", "second", "second-native"
	row.State = domain.SideCompactionPending
	row.Scope = nil
	var content durableSideContent
	if err := json.Unmarshal(row.Content, &content); err != nil {
		t.Fatal(err)
	}
	content.Record.Side.ID = "second"
	content.Record.Side.ProviderForkID = "second-native"
	row.Content, _ = json.Marshal(content)
	if err := db.RegisterSideCompaction(t.Context(), row); err != nil {
		t.Fatal(err)
	}
	if err := svc.PreflightShutdown(t.Context()); !errors.Is(err, ports.ErrChatShutdownNotSettled) {
		t.Fatal(err)
	}
	if err := svc.closeSideCompactions(t.Context(), "second"); err != nil {
		t.Fatal(err)
	}
	writer := recordWriter(t, svc)
	if _, err := writer.Exec(`CREATE TRIGGER reject_flush BEFORE UPDATE ON chat_side_contents BEGIN SELECT RAISE(ABORT,'no content commit'); END`); err != nil {
		t.Fatal(err)
	}
	var sideErr *ports.ChatShutdownSideError
	if err := svc.PreflightShutdown(t.Context()); !errors.As(err, &sideErr) || sideErr.Reason != "side_content_not_durable" {
		t.Fatalf("flush failure: %v", err)
	}
	if _, err := writer.Exec(`DROP TRIGGER reject_flush`); err != nil {
		t.Fatal(err)
	}
	if err := svc.PreflightShutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestHistoricalBoundaryAtomicAndNormalResultWins(t *testing.T) {
	for _, normalFirst := range []bool{false, true} {
		name := "history_write_failure"
		if normalFirst {
			name = "normal_result_first"
		}
		t.Run(name, func(t *testing.T) {
			scope, stop := historicalScopeHost(t)
			svc, db, conv := historicalSide(t, scope, domain.HarnessClaudeCode, domain.PermissionModeDefault)
			stop()
			before := onlyCompaction(t, db)
			if normalFirst {
				conv.idle.Store(true)
				if err := svc.PreflightShutdown(t.Context()); err != nil {
					t.Fatal(err)
				}
				after := onlyCompaction(t, db)
				if after.State != domain.SideCompactionSettled || after.Historical || len(after.Content) != 0 {
					t.Fatalf("normal result lost: %+v", after)
				}
				if err := db.MarkSideCompactionHistorical(t.Context(), after.ID, after.Epoch, "scope_boot_changed"); !errors.Is(err, domain.ErrSideCompactionTransition) {
					t.Fatal(err)
				}
				return
			}
			writer := recordWriter(t, svc)
			if _, err := writer.Exec(`CREATE TRIGGER reject_history BEFORE UPDATE OF record_json ON chat_side_compactions BEGIN SELECT RAISE(ABORT,'no history commit'); END`); err != nil {
				t.Fatal(err)
			}
			if err := svc.PreflightShutdown(t.Context()); !errors.Is(err, ports.ErrChatShutdownNotSettled) {
				t.Fatal(err)
			}
			after := onlyCompaction(t, db)
			if after.Historical || after.ScopeEndReason != "" || after.State != before.State {
				t.Fatalf("partial history: %+v", after)
			}
			if _, err := writer.Exec(`DROP TRIGGER reject_history`); err != nil {
				t.Fatal(err)
			}
			if err := svc.PreflightShutdown(t.Context()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestProtectedSideRecoverabilityMatchesPreflight(t *testing.T) {
	for _, kind := range []string{"valid", "empty_record", "missing_turn_image", "missing_draft_image"} {
		t.Run(kind, func(t *testing.T) {
			scope, stop := historicalScopeHost(t)
			svc, db, _ := historicalSide(t, scope, domain.HarnessCodex, domain.PermissionModeAcceptEdits)
			store := svc.sides.store.(*memorySideStore)
			if err := store.SetSideDraft(t.Context(), "side", "run", receiptDraft, time.Now()); err != nil {
				t.Fatal(err)
			}
			turn := domain.SideTurn{ID: "question", SideID: "side", ClientMessageID: "image-question", Text: "saved question", Content: []domain.SideContent{{Type: "image", MIMEType: "image/png", Data: "dHVybiBpbWFnZQ=="}}, CreatedAt: time.Now()}
			if _, _, err := store.ReserveSideTurn(t.Context(), turn, "run"); err != nil {
				t.Fatal(err)
			}
			row := onlyCompaction(t, db)
			var saved durableSideContent
			if err := json.Unmarshal(row.Content, &saved); err != nil {
				t.Fatal(err)
			}
			if kind == "empty_record" {
				if _, err := recordWriter(t, svc).ExecContext(t.Context(), "UPDATE chat_side_contents SET content_json='{}'"); err != nil {
					t.Fatal(err)
				}
			} else if kind != "valid" {
				name := saved.Attachments["question:0"]
				if kind == "missing_draft_image" {
					for _, image := range saved.DraftImages {
						name = image
						break
					}
				}
				if name == "" {
					t.Fatal("image fixture missing")
				}
				if err := os.Remove(filepath.Join(svc.dataDir, "attachments", saved.Record.ProviderHostID, name)); err != nil {
					t.Fatal(err)
				}
			}
			stop()
			restarted := New(Options{Store: db, DataDir: svc.dataDir, AppRunID: "new-launch", Sessions: sideSessionReader{}})
			t.Cleanup(restarted.sides.cancel)
			if err := restarted.InitializeSideChats(t.Context()); err != nil {
				t.Fatal(err)
			}
			before := onlyCompaction(t, db)
			list, err := restarted.ListProtectedSides(t.Context())
			if err != nil || len(list) != 1 {
				t.Fatalf("list=%+v err=%v", list, err)
			}
			if after := onlyCompaction(t, db); !reflect.DeepEqual(before, after) {
				t.Fatal("list changed durable metadata or contents")
			}
			preflight := restarted.PreflightShutdown(t.Context())
			blocked := kind != "valid"
			var sideErr *ports.ChatShutdownSideError
			if blocked && (!errors.As(preflight, &sideErr) || sideErr.Reason != "side_content_not_durable") || !blocked && preflight != nil {
				t.Fatalf("unexpected preflight: %v", preflight)
			}
			if list[0].Protection.ShutdownBlocked != blocked || blocked && list[0].Protection.Reason != sideErr.Reason {
				t.Fatalf("list shutdownBlocked=%v reason=%q; preflight=%v; want blocked=%v", list[0].Protection.ShutdownBlocked, list[0].Protection.Reason, preflight, blocked)
			}
		})
	}
}
