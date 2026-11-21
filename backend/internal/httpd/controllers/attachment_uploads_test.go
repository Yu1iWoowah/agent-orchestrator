package controllers_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/attachmentstore"
	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/requestscope"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func uploadTestRouter(svc controllers.SessionService, dataDir string) http.Handler {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Config{DataDir: dataDir, AllowedOrigins: []string{"app://renderer"}}
	return httpd.NewRouterWithControl(cfg, log, nil, httpd.APIDeps{Sessions: svc}, httpd.ControlDeps{})
}

func TestAttachmentUploadStreamsBodyAndSpawnNamesIt(t *testing.T) {
	svc := newFakeSessionService()
	dataDir := t.TempDir()
	srv := httptest.NewServer(uploadTestRouter(svc, dataDir))
	defer srv.Close()

	// Larger than the old 50 MiB per-file cap, and than the inline body limit.
	const size = 150 << 20
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/attachment-uploads?projectId=ao", io.LimitReader(zeros{}, size))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload status = %d, want 201; body=%s", resp.StatusCode, body)
	}
	var uploaded struct {
		UploadID string `json:"uploadId"`
		Bytes    int64  `json:"bytes"`
	}
	mustJSON(t, body, &uploaded)
	if uploaded.Bytes != size || !strings.HasPrefix(uploaded.UploadID, "upload-") {
		t.Fatalf("upload = %+v, want %d bytes", uploaded, size)
	}
	spawn, _ := json.Marshal(map[string]any{
		"projectId": "ao", "kind": "worker", "harness": "codex", "prompt": "inspect the video",
		"attachments": []map[string]string{{"mimeType": "video/mp4", "uploadId": uploaded.UploadID}},
	})
	resBody, status, _ := doRequest(t, srv, http.MethodPost, "/api/v1/sessions", string(spawn))
	if status != http.StatusCreated {
		t.Fatalf("spawn naming an upload = %d, want 201; body=%s", status, resBody)
	}
	got := svc.lastSpawn.Attachments
	if len(got) != 1 || got[0].Upload == "" || got[0].Ext != ".mp4" || got[0].Data != nil {
		t.Fatalf("spawn attachments = %+v", got)
	}
	info, err := os.Stat(filepath.Join(dataDir, "attachment-uploads", got[0].Upload))
	if err != nil || info.Size() != size {
		t.Fatalf("uploaded file = %v, %v", info, err)
	}
}

func TestAttachmentUploadRejectsEmptyBody(t *testing.T) {
	srv := httptest.NewServer(uploadTestRouter(newFakeSessionService(), t.TempDir()))
	defer srv.Close()
	body, status, _ := doRequest(t, srv, http.MethodPost, "/api/v1/attachment-uploads", "")
	assertErrorCode(t, body, status, http.StatusBadRequest, "INVALID_ATTACHMENT_DATA")
}

type uploadStageService struct {
	*fakeSessionService
	store *attachmentstore.Store
}

func (s uploadStageService) StageAttachments(ctx context.Context, id domain.SessionID, attachments []ports.SpawnAttachment) ([]string, error) {
	name := "attachment-staged.txt"
	if err := s.store.PutCanonicalSource(ctx, id, name, attachmentstore.Source{Upload: attachments[0].Upload}); err != nil {
		return nil, err
	}
	return []string{name}, nil
}

func TestAttachmentUploadStagesOnlyForItsSessionAndCredential(t *testing.T) {
	dataDir := t.TempDir()
	store := attachmentstore.New(dataDir)
	svc := uploadStageService{fakeSessionService: newFakeSessionService(), store: store}
	router := uploadTestRouter(svc, dataDir)
	post := func(path, body, deviceID string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		if deviceID != "" {
			req = req.WithContext(requestscope.WithCredential(req.Context(), requestscope.Credential{Kind: requestscope.CredentialDevice, DeviceID: deviceID}))
		}
		res := httptest.NewRecorder()
		router.ServeHTTP(res, req)
		return res
	}
	const contents = "session A attachment"
	uploaded := post("/api/v1/attachment-uploads?sessionId=ao-1", contents, "device-a")
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("upload status = %d: %s", uploaded.Code, uploaded.Body)
	}
	var result controllers.AttachmentUploadResponse
	mustJSON(t, uploaded.Body.Bytes(), &result)
	stage := `{"attachments":[{"mimeType":"text/plain","uploadId":"` + result.UploadID + `"}]}`
	for _, tc := range []struct{ session, device string }{{"ao-2", "device-a"}, {"ao-1", "device-b"}, {"ao-1", ""}} {
		res := post("/api/v1/sessions/"+tc.session+"/attachments", stage, tc.device)
		assertErrorCode(t, res.Body.Bytes(), res.Code, http.StatusBadRequest, "ATTACHMENT_UPLOAD_NOT_FOUND")
	}
	res := post("/api/v1/sessions/ao-1/attachments", stage, "device-a")
	if res.Code != http.StatusCreated {
		t.Fatalf("stage status = %d: %s", res.Code, res.Body)
	}
	file, _, err := store.Open(context.Background(), "ao-1", "attachment-staged.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if got, err := io.ReadAll(file); err != nil || string(got) != contents {
		t.Fatalf("staged content = %q, %v", got, err)
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}
