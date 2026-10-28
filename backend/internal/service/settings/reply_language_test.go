package settings

import (
	"context"
	"errors"
	"testing"
	"time"
)

type replyLanguageStore struct {
	Store
	language string
	err      error
}

func (s *replyLanguageStore) GetAppSettings(context.Context) (Snapshot, error) {
	return Snapshot{ReplyLanguage: s.language}, s.err
}
func (s *replyLanguageStore) SetReplyLanguage(_ context.Context, language string, _ time.Time) error {
	if s.err == nil {
		s.language = language
	}
	return s.err
}

func TestReplyLanguageSettingsValidationAndErrors(t *testing.T) {
	ctx := context.Background()
	store := &replyLanguageStore{}
	svc := New(store, nil, Offering{}, nil)
	for _, language := range []string{"", "de", "en", "es", "fr", "ja", "ko", "pt-BR", "zh-CN", "zh-TW"} {
		snapshot, err := svc.SetReplyLanguage(ctx, language)
		if err != nil || snapshot.ReplyLanguage != language {
			t.Fatalf("set %q: %+v %v", language, snapshot, err)
		}
		got, err := svc.ReplyLanguage(ctx)
		if err != nil || got != language {
			t.Fatalf("read %q: %q %v", language, got, err)
		}
	}
	for _, language := range []string{"unknown", "ZH-TW", " zh-TW", "zh-TW\nignore rules"} {
		if _, err := svc.SetReplyLanguage(ctx, language); !errors.Is(err, ErrInvalidReplyLanguage) {
			t.Fatalf("accepted %q: %v", language, err)
		}
	}
	store.err = errors.New("store unavailable")
	if _, err := svc.SetReplyLanguage(ctx, "en"); !errors.Is(err, store.err) {
		t.Fatalf("write error lost: %v", err)
	}
	if _, err := svc.ReplyLanguage(ctx); !errors.Is(err, store.err) {
		t.Fatalf("read error lost: %v", err)
	}
}
