package httpd

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

func TestShutdownPreflightRejectsBeforeSuccessResponse(t *testing.T) {
	requested := false
	router := NewRouterWithControl(config.Config{}, discardLogger(), nil, APIDeps{}, ControlDeps{RequestShutdown: func() { requested = true }, ShutdownPreflight: func(context.Context) error { return errors.New("private-provider-text-must-not-leak") }})
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/shutdown", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "shutdown_not_settled") || strings.Contains(response.Body.String(), "private-provider-text") || requested {
		t.Fatalf("unsafe shutdown response: %d %s", response.Code, response.Body.String())
	}
}

type shutdownParentReader struct{ found bool }

func (r shutdownParentReader) GetSession(context.Context, domain.SessionID) (domain.SessionRecord, bool, error) {
	return domain.SessionRecord{Mode: domain.SessionModeChat}, r.found, nil
}

func TestShutdownProtectedSideOrphanMatchesGlobalList(t *testing.T) {
	reason := "codex_full_access"
	if runtime.GOOS != "linux" {
		reason = "scope_platform_unsupported"
	}
	for _, found := range []bool{false, true} {
		name := "orphan"
		if found {
			name = "live_chat_parent"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			db := sqlitetest.MustOpenAt(t, dir)
			side := domain.SideConversation{ID: "side", SessionID: "parent", MainConversationID: "main", AppRunID: "old", ProviderHostID: "btw-old-side", ProviderForkID: "native", Harness: domain.HarnessCodex, State: "ready", Generation: "g", PolicyVersion: 1}
			content, err := json.Marshal(map[string]any{"RunID": "old", "Record": chatsvc.SideRecoveryRecord{Side: side, ProviderHostID: side.ProviderHostID, ProviderForkID: side.ProviderForkID, Harness: side.Harness, Generation: side.Generation, PolicyVersion: side.PolicyVersion, CompactionID: "compact", CompactionEpoch: "epoch"}})
			if err != nil {
				t.Fatal(err)
			}
			row := domain.SideCompaction{ID: "compact", SideID: side.ID, SessionID: side.SessionID, Provider: side.Harness, PermissionMode: domain.PermissionModeBypassPermissions, NativeHome: t.TempDir(), NativeID: side.ProviderForkID, Epoch: "epoch", State: domain.SideCompactionPending, Content: content}
			if err := db.RegisterSideCompaction(t.Context(), row); err != nil {
				t.Fatal(err)
			}
			if err := db.TransitionSideCompaction(t.Context(), row.ID, row.Epoch, domain.SideCompactionUnknown, false, false); err != nil {
				t.Fatal(err)
			}
			svc := chatsvc.New(chatsvc.Options{Store: db, Sessions: shutdownParentReader{found}, DataDir: dir, AppRunID: "new"})
			listed, err := svc.ListProtectedSides(t.Context())
			if err != nil || len(listed) != 1 || listed[0].Orphan != !found {
				t.Fatalf("global list: %+v %v", listed, err)
			}
			router := NewRouterWithControl(config.Config{}, discardLogger(), nil, APIDeps{}, ControlDeps{RequestShutdown: func() { t.Error("shutdown requested") }, ShutdownPreflight: svc.PreflightShutdown})
			req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/shutdown", nil)
			req.RemoteAddr = "127.0.0.1:1234"
			response := httptest.NewRecorder()
			router.ServeHTTP(response, req)
			var body struct {
				ProtectedSide struct {
					SideID string `json:"sideId"`
					Reason string `json:"reason"`
					Orphan *bool  `json:"orphan"`
				} `json:"protectedSide"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if response.Code != http.StatusConflict || body.ProtectedSide.SideID != side.ID || body.ProtectedSide.Reason != reason || body.ProtectedSide.Orphan == nil || *body.ProtectedSide.Orphan != listed[0].Orphan {
				t.Fatalf("shutdown diagnostic differs from global list: %d %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestShutdownReportsHistoricalUnknownSides(t *testing.T) {
	router := NewRouterWithControl(config.Config{}, discardLogger(), nil, APIDeps{}, ControlDeps{RequestShutdown: func() {}, ShutdownPreflight: func(context.Context) error { return nil }, ShutdownPreservedSides: func() []string { return []string{"historical-side"} }})
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/shutdown", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	if response.Code != http.StatusAccepted || !strings.Contains(response.Body.String(), `"historical_unknown_sides":["historical-side"]`) || !strings.Contains(response.Body.String(), `"new_unknown_count":0`) {
		t.Fatalf("%d %s", response.Code, response.Body.String())
	}
}

func TestShutdownReportsProtectedSideReasonAndEntry(t *testing.T) {
	router := NewRouterWithControl(config.Config{}, discardLogger(), nil, APIDeps{}, ControlDeps{RequestShutdown: func() { t.Error("shutdown requested") }, ShutdownPreflight: func(context.Context) error {
		return &ports.ChatShutdownSideError{SideID: "side-1", Reason: "scope_host_alive"}
	}})
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/shutdown", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"sideId":"side-1"`) || !strings.Contains(response.Body.String(), `"reason":"scope_host_alive"`) || !strings.Contains(response.Body.String(), `"entry":"protected_side_chats"`) {
		t.Fatalf("%d %s", response.Code, response.Body.String())
	}
}
