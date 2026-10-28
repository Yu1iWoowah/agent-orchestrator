package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

func TestReplyLanguagePersistsAndDisables(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := sqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := s.GetAppSettings(ctx)
	if err != nil || initial.ReplyLanguage != "" {
		t.Fatalf("default = %+v, %v", initial, err)
	}
	if err := s.SetReplyLanguage(ctx, "zh-TW", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = sqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	saved, err := s.GetAppSettings(ctx)
	if err != nil || saved.ReplyLanguage != "zh-TW" {
		t.Fatalf("reopened = %+v, %v", saved, err)
	}
	if saved.DefaultSessionMode != initial.DefaultSessionMode || saved.VersionRefresh != initial.VersionRefresh {
		t.Fatal("changed unrelated settings")
	}
	if err := s.SetReplyLanguage(ctx, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	saved, err = s.GetAppSettings(ctx)
	if err != nil || saved.ReplyLanguage != "" {
		t.Fatalf("disabled = %+v, %v", saved, err)
	}
}
