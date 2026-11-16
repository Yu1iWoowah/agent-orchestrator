package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/cloud/internal/domain"
)

// The desktop upload/list tests consume this same wire fixture. Keep it
// checked against the real fork router, including its absent multipart route.
func TestArtifactShareDesktopContract(t *testing.T) {
	raw, err := os.ReadFile("testdata/artifact-share-desktop.json")
	if err != nil {
		t.Fatal(err)
	}
	var contract struct {
		UnsupportedMultipart struct {
			Status int    `json:"status"`
			Body   string `json:"body"`
		} `json:"unsupportedMultipart"`
		MaxBytes  int             `json:"maxBytes"`
		Publish   json.RawMessage `json:"publish"`
		Republish json.RawMessage `json:"republish"`
		List      json.RawMessage `json:"list"`
	}
	if err := json.Unmarshal(raw, &contract); err != nil {
		t.Fatal(err)
	}
	if contract.MaxBytes != domain.MaxArtifactShareBytes {
		t.Fatalf("desktop inline upload limit = %d, router limit = %d", contract.MaxBytes, domain.MaxArtifactShareBytes)
	}
	h := newShareHarness(t, &ArtifactShareOptions{Origin: shareOrigin, Limit: 3})
	missing := h.request(http.MethodPost, "/api/cloud/v1/me/artifact-shares/uploads", "", artifactTokenA, []byte(`{}`), nil)
	if missing.Code != contract.UnsupportedMultipart.Status || missing.Body.String() != contract.UnsupportedMultipart.Body {
		t.Fatalf("multipart route: %d %q", missing.Code, missing.Body.String())
	}
	published := h.publish(artifactTokenA, "upload.txt", "p", "f", []byte("v1"))
	share := decodeBody[struct {
		Share artifactShareResponse `json:"share"`
	}](t, published).Share
	if requireUUID(share.ID, "id") != nil {
		t.Fatalf("publish did not return a share ID: %s", published.Body.String())
	}
	check := func(rec *httptest.ResponseRecorder, status int, want json.RawMessage) {
		t.Helper()
		if rec.Code != status {
			t.Fatalf("status = %d, want %d: %s", rec.Code, status, rec.Body.String())
		}
		// UUID creation is the only nondeterministic field in this harness.
		body := strings.ReplaceAll(rec.Body.String(), share.ID, "00000000-0000-4000-8000-000000000001")
		var gotValue, wantValue any
		if err := json.Unmarshal([]byte(body), &gotValue); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(want, &wantValue); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(gotValue, wantValue) {
			t.Fatalf("router response differs from desktop fixture:\ngot %s\nwant %s", body, want)
		}
	}
	check(published, http.StatusCreated, contract.Publish)
	check(h.republish(artifactTokenA, share.ID, "upload.txt", []byte("v2")), http.StatusOK, contract.Republish)
	check(h.request(http.MethodGet, "/api/cloud/v1/me/artifact-shares", "", artifactTokenA, nil, nil), http.StatusOK, contract.List)
}
