package controllers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	settingssvc "github.com/aoagents/agent-orchestrator/backend/internal/service/settings"
)

type replyLanguageStore struct {
	settingssvc.Store
	snapshot settingssvc.Snapshot
}

func (s *replyLanguageStore) GetAppSettings(context.Context) (settingssvc.Snapshot, error) {
	return s.snapshot, nil
}
func (s *replyLanguageStore) SetReplyLanguage(_ context.Context, language string, _ time.Time) error {
	s.snapshot.ReplyLanguage = language
	return nil
}

func TestSettingsReplyLanguageAPI(t *testing.T) {
	store := &replyLanguageStore{}
	c := &SettingsController{Svc: settingssvc.New(store, nil, settingssvc.Offering{}, nil)}
	r := chi.NewRouter()
	c.Register(r)
	for _, body := range []string{`{}`, `{"replyLanguage":null}`, `{"replyLanguage":true}`, `{"replyLanguage":"unknown"}`} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodPatch, "/settings/reply-language", strings.NewReader(body)))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("body=%s status=%d response=%s", body, w.Code, w.Body)
		}
	}
	for _, language := range []string{"zh-TW", "en", "de", "es", "fr", "ja", "ko", "pt-BR", "zh-CN", ""} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodPatch, "/settings/reply-language", strings.NewReader(`{"replyLanguage":"`+language+`"}`)))
		if w.Code != http.StatusOK || store.snapshot.ReplyLanguage != language {
			t.Fatalf("save %q: status=%d body=%s", language, w.Code, w.Body)
		}
		w = httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/settings", nil))
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"replyLanguage":"`+language+`"`) {
			t.Fatalf("read %q: %s", language, w.Body)
		}
	}
}
